/*
 * ============================================================================
 * Results -- the second arrow into the system
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumer: `ingest`'s results poller. The seam it backs is a consumer-declared
 * ResultsProvider in internal/ingest; the rows it writes are read at the far end
 * by settlement.sql's ListFinalisedEventsSince, which is `settle`'s ResultsSource
 * and which needed no change to start working.
 *
 * WHY RESULTS ARE THEIR OWN SOURCE AND NOT A FIELD ON THE ODDS PATH
 * ------------------------------------------------------------------
 * CLAUDE.md section 3 draws two arrows into the pipeline, not one:
 *
 *     provider -> ingest -> [odds.raw.*] -> normalizer -> [odds.normalized] -> ...
 *     results  -> settle -> [wager.events] -> ledger -> ...
 *
 * `results` is drawn as an INPUT TO SETTLE, beside the odds flow rather than
 * carried on it, and this file is the storage end of that second arrow. The
 * separation is structural, and both halves of the argument matter because the
 * obvious fix -- "just put the score on the normalized record" -- is the wrong
 * one twice over:
 *
 *   1. THE ODDS PATH CANNOT CARRY A RESULT EVEN IN PRINCIPLE. odds.normalized is
 *      compacted and KEYED BY MARKET, and a finished contest has no priced
 *      market to key on: the books take their prices down when play ends, the
 *      synthetic adapter models exactly that, and normalizer/mapper.go rejects a
 *      payload with no observation instant on it. An ended event is precisely
 *      the shape that produces no record. There is nothing to hang the result
 *      on.
 *
 *   2. THE NORMALIZER'S EXCLUSION OF SCORE AND CLOCK IS CORRECT AND STAYS.
 *      normalizer/payload.go leaves both out of the published record on purpose.
 *      A record carrying a live score would be republished for every market on
 *      the event on every score change -- the exact bus flood that change
 *      detection exists to prevent -- and it would do so at the moment the slate
 *      is busiest. Relaxing that to reach settlement would trade a correct
 *      design for a convenient one.
 *
 * So a result travels the short way: `ingest` asks the provider for the outcome
 * of a contest whose start time is far enough in the past that it must be over,
 * and writes the terminal status and final score straight onto the `events` row.
 *
 * THIS FILE IS THE SEAM A REAL PROVIDER LANDS ON
 * ----------------------------------------------
 * CLAUDE.md section 13 leaves the odds provider undecided, and every candidate
 * exposes scores on a SEPARATE endpoint from prices -- a scores/results route,
 * polled at its own cadence, returning finished fixtures. That shape is what
 * these two statements are: a work queue of contests that should have finished,
 * and an idempotent write of the outcome one of them came back with. Choosing a
 * provider changes which adapter fills the queue's answers; it changes nothing
 * here.
 *
 * NO MOCK DATA
 * ------------
 * Nothing in this file invents a result, and neither does the shipped provider
 * behind it. synthetic/model.go's scoreAt is a pure deterministic function of
 * the event's static means and a seeded pace draw -- THE SAME latent process
 * that prices the market -- so the score and the line are two views of one
 * simulated contest rather than two independent inventions. Asking that
 * generator for the final score is reading the generator's own output, which is
 * what the odds path already does with its prices.
 *
 * The mechanical form of that claim is that UpsertEventResult is an UPDATE and
 * never an INSERT. A result for a contest this deployment never ingested cannot
 * create a contest, so there is no route by which this file adds a row to the
 * catalogue.
 *
 * WHY THESE TWO STATEMENTS ARE HERE AND NOT IN catalogue.sql
 * ----------------------------------------------------------
 * catalogue.sql is a READ file whose queries are shared between the API and
 * ingest's polling scheduler, and its own header says catalogue WRITES belong to
 * ingest's upsert path. These are writes, and they are not the catalogue's: the
 * catalogue upsert asserts what a provider says an event IS, on every poll,
 * across sixteen columns. This statement asserts what a contest FINALLY WAS,
 * once, across four -- and it must not be able to touch the other twelve. Two
 * different questions, two different guards, two different files.
 * ============================================================================
 */


-- Write one finished contest's terminal status and final score.
--
-- WHY THE NAME SAYS UPSERT AND THE STATEMENT IS AN UPDATE. The name describes
-- what the CALLER gets: a converging, replay-safe write it may issue on every
-- sweep without keeping a memo of what it has already recorded. It is not an
-- INSERT ... ON CONFLICT, and it must not become one. The row is absent in
-- exactly one situation -- a contest that finished before this deployment ever
-- saw it -- and inserting then would put an event on the books with no league
-- guaranteed present, no markets, and no legs riding on it: a row settlement
-- would read, find nothing to grade, and re-read on every poll for ever.
-- Declining to write it loses nothing, because there is no wager on a game we
-- never listed.
--
-- FOUR COLUMNS WRITTEN, TWELVE UNTOUCHED. league_id, kind, name, both
-- competitors, scheduled_start and the timestamps are the CATALOGUE's, asserted
-- by the odds path's upsert on every poll. Naming them here would let a results
-- payload -- which knows only an outcome -- overwrite the slate with whatever
-- default it happened to carry. The statement names status, the two score
-- columns, the three clock columns and observed_at, and nothing else.
--
-- THE CLOCK IS CLEARED, NOT LEFT ALONE. events_clock_only_in_play (00002)
-- refuses a clock on a row that is not in play, so a live row carrying a period
-- and an elapsed time would fail the CHECK the instant its status became
-- 'ended' unless the three columns were nulled in the same statement.
-- events_clock_all_or_nothing requires all three or none, which is why it is all
-- three. Clearing them is also right on its own terms -- domain.Event.WithStatus
-- does the same thing, because "an ended event that still reported 'Q3 7:34'
-- would be a lie that the UI would happily render".
--
-- The two score columns move together for the same reason:
-- events_score_all_or_nothing constrains the PAIR. Both are sqlc.narg because a
-- cancelled contest is a result WITHOUT a score -- it will not be played, every
-- leg voids, every stake comes back -- and settlement.sql's feed deliberately
-- includes it.
--
-- THE TWO GUARDS
--
--   status NOT IN ('ended','settled','cancelled') is the no-going-backwards
--   rule, and it is written as the exact complement of the status set
--   ListFinalisedEventsSince reads. Stated once: THIS STATEMENT ONLY EVER MOVES
--   A ROW INTO THE RESULTS FEED, NEVER OUT OF IT AND NEVER WITHIN IT. So a
--   result cannot un-settle an event whose wagers `settle` has already graded
--   and paid, and cannot restate a cancellation as an ended game after the
--   voids have gone through the ledger. `settled` in particular is the domain's
--   only terminal status (domain.EventStatus) and nothing may write past it.
--
--   observed_at <= @observed_at is the out-of-order guard, spelled the way
--   catalogue.go's upsertEvent spells it and the way the column comment on
--   events.observed_at requires. A redelivery, a replay, or two ingest replicas
--   racing must not overwrite a newer observation with an older one.
--
-- The `IS DISTINCT FROM` clause that guards every catalogue upsert is
-- deliberately ABSENT, and its absence is not an oversight. Its job there is to
-- make the steady state -- the same payload re-asserted on every poll -- write
-- nothing. Here the status guard already does that and does it more strongly: a
-- replay finds the row already in the results feed and matches zero rows before
-- any column is compared. Adding the clause would be a second guard for a case
-- the first one cannot reach.
--
-- :execrows, and the caller MUST check the count. One means this call was the
-- one that recorded the result and the wagers on the event are now settleable.
-- ZERO IS NOT AN ERROR: it means the result was already recorded, or a newer
-- observation is stored, or this deployment never ingested the contest. All
-- three are steady states a poller meets constantly, which is what lets it call
-- this on every sweep for as long as the contest stays on the provider's slate.
-- The row count is the only signal that separates them from a write that landed;
-- `:exec` would discard it, and `:one` with RETURNING would collapse all three
-- into pgx.ErrNoRows -- the same trade settlement.sql's header rejects at
-- length.
--
-- observed_at IS THE PROVIDER'S INSTANT, never now() and never the ingest
-- container's clock. settlement stamps every leg it grades from this row with
-- it, so a replayed result must re-apply the ORIGINAL observation time rather
-- than the replay's.
--
-- ON THE PLAN: primary-key lookup, one row.
--
-- name: UpsertEventResult :execrows
UPDATE events
   SET status           = @status,
       score_home       = sqlc.narg(score_home)::INTEGER,
       score_away       = sqlc.narg(score_away)::INTEGER,
       clock_period     = NULL,
       clock_elapsed_ns = NULL,
       clock_running    = NULL,
       observed_at      = @observed_at
 WHERE id = @id
   AND status NOT IN ('ended', 'settled', 'cancelled')
   AND observed_at <= @observed_at;


-- The results poller's work queue: contests that should be over and are not yet
-- recorded as such.
--
-- This is the question a results endpoint is polled with -- "which fixtures do I
-- need an outcome for?" -- and it is asked of the database rather than kept in
-- memory because the answer must survive a restart. A poller holding its own set
-- of pending events would lose it on every deploy, and every contest that
-- finished during the gap would go unsettled with no record that it had been
-- missed.
--
-- THE PREDICATE IS `status IN ('scheduled','live','suspended')`, WRITTEN OUT.
-- Two reasons, and the first is mechanical: those three literals are the exact
-- predicate of the PARTIAL index events_open_start_idx, so the planner can prove
-- the index covers this query. Parameterising the set, or writing it as the
-- complement `NOT IN ('ended','settled','cancelled','postponed')`, silently
-- loses the index and turns the highest-value read on the results path into a
-- sequential scan of the whole slate. catalogue.sql's header states the same
-- rule for the board queries.
--
-- The second is semantic: those three are exactly the statuses of a contest that
-- has not yet produced an outcome. `postponed` IS DELIBERATELY ABSENT and is the
-- only interesting exclusion. A postponed event is not awaiting a RESULT, it is
-- awaiting a new START TIME -- domain.EventStatus admits `postponed ->
-- scheduled` -- and its old scheduled_start recedes further into the past every
-- day, so including it would park it permanently at the head of this queue and
-- ask the provider for the score of a game that has not been played.
-- settlement.sql's feed excludes it at the far end for the matching reason.
-- (A postponed contest that is ultimately abandoned still settles: the provider
-- pushes `cancelled` down the odds path's own event upsert, and
-- UpsertEventResult accepts that source status too. Only the QUEUE is narrow.)
--
-- @finished_before IS A HORIZON THE CALLER COMPUTES, not now() and not a
-- hardcoded interval. It is the instant before which a contest that started must
-- have ended -- in practice now() minus a typical contest duration plus a margin
-- -- and it belongs to the caller for the reason every instant in this schema
-- does: a query that reads the database's clock cannot be tested at a fixed time
-- and cannot be replayed. catalogue.sql's ListOpenEventsStartingBefore makes the
-- same choice for the polling horizon and says so.
--
-- `kind` IS RETURNED AND NOT FILTERED. An outright ('2027 NBA Champion') carries
-- a scheduled_start too, and by this predicate a season-long futures market
-- looks finished the day after it opens. It is still not filtered out here,
-- because an outright genuinely does resolve and its wagers genuinely do need
-- settling -- excluding it in SQL would make that unreachable for ever. What
-- differs between a match and a season is HOW LONG AFTER THE START the outcome
-- exists, and that horizon is the provider's knowledge, not this statement's.
-- The caller reads `kind` and decides.
--
-- ORDERED BY (scheduled_start, id) so the ordering is TOTAL: `id` is the primary
-- key, so two fixtures kicking off at the same instant cannot be returned in an
-- arbitrary order that differs between two executions. Oldest first, so the
-- contest that has been waiting longest -- the one with a customer's stake held
-- in escrow the longest -- is polled first.
--
-- BOUNDED BY @row_limit for the reason every list in this surface is: one poll
-- must not be able to pull an unbounded slate into memory. The queue drains
-- across sweeps, and it drains oldest-first, so a bound costs latency on a
-- backlog and never correctness.
--
-- ON THE PLAN: index scan of events_open_start_idx (partial, on scheduled_start)
-- with the bound as the range, already in ORDER BY order; only the `id` tiebreak
-- among ties needs a sort.
--
-- name: ListEventsAwaitingResult :many
SELECT id,
       league_id,
       kind,
       name,
       status,
       scheduled_start,
       observed_at
  FROM events
 WHERE status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < @finished_before
 ORDER BY scheduled_start, id
 LIMIT @row_limit;
