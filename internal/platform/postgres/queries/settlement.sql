/*
 * ============================================================================
 * Grading and settlement -- the UPDATE half of phase 8
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumer: `settle` (CLAUDE.md phase 8), plus the cash-out path in `api`, which
 * is a settlement the customer asks for rather than one a result forces. The
 * seams these back are declared in internal/settlement/ports.go
 * (ResultsSource, Store, Tx) and internal/betting/ports.go.
 *
 * WHY THIS IS A SEPARATE FILE FROM betting.sql
 * --------------------------------------------
 * Not because two files are tidier. Because the two halves of phase 8 are
 * governed by opposite mechanisms, and mixing them would make the shared rule of
 * each file unstatable.
 *
 * betting.sql is INSERT work under DEFERRED constraint triggers. A placement
 * writes a wager, its legs, a ledger transaction and its entries, and the
 * invariants that judge it -- wagers_shape_at_commit, legs_shape_at_commit,
 * ledger_entries_balanced_at_commit -- are all DEFERRABLE INITIALLY DEFERRED and
 * all fire at COMMIT. The rule for that file is: EVERY STATEMENT SUCCEEDS AND
 * THE COMMIT DECIDES. Its writes are `:exec`, because there is nothing useful
 * for a caller to check per row.
 *
 * settlement.sql is UPDATE work under IMMEDIATE row triggers. Every statement
 * here is judged the instant it runs, by wagers_assert_transition or
 * legs_assert_transition (00006), which enforce the domain's own state machines:
 * a graded leg is terminal, a settled wager is terminal, the booked terms are
 * immutable, the returned amount is write-once, and transitioned_at is monotone.
 * The rule for this file is: EVERY UPDATE IS GUARDED AND THE CALLER CHECKS
 * ROWS-AFFECTED. Its writes are `:execrows`, and that is not decoration.
 *
 * EVERY UPDATE IS GUARDED BY ITS EXPECTED CURRENT STATUS. THIS IS THE
 * CONCURRENCY CONTROL FOR THE STATUS TRANSITION ITSELF.
 * ---------------------------------------------------------------------
 * Each UPDATE below carries the status it expects to find in its WHERE clause
 * (`AND status = 'pending'`, `AND status IN ('placed','open')`), and the caller
 * MUST inspect the row count. Zero means somebody else already did this work.
 *
 * That is a complete control for the transition, and the reason is a PostgreSQL
 * detail worth writing down because it is the thing people assume they need a
 * lock for. Under READ COMMITTED, when two transactions run the same guarded
 * UPDATE, the second BLOCKS on the row lock the first took; when the first
 * commits, the second does not proceed with its original snapshot -- it
 * re-evaluates its WHERE clause against the NEW row version (EvalPlanQual) and
 * updates nothing if the guard no longer holds. So the loser of a race sees
 * rows-affected = 0 and cannot double-settle.
 *
 * A `:exec` variant of any of them would discard exactly the signal that makes
 * this work. `:one` with RETURNING was considered and rejected: RETURNING yields
 * no row when nothing matched, which collapses "already settled" into
 * pgx.ErrNoRows and makes it indistinguishable from "no such wager".
 * internal/settlement/ports.go puts it more bluntly: "Returning nil from either
 * on a zero-row update is the single most dangerous thing an implementation of
 * this interface can do, because the ledger write that follows would balance
 * perfectly and pay twice."
 *
 * THE ONE LOCK IN THIS FILE, AND WHY IT IS NOT THE CONTROL
 * --------------------------------------------------------
 * GetWagerForSettlement takes FOR UPDATE. It is not what prevents a double
 * payout -- the guarded UPDATE and the write-once returned amount already do
 * that -- it is what makes two settle replicas grading two different events of
 * one parlay WAIT for each other instead of racing to a commit that one of them
 * loses. ports.go states the trade: without it "they still cannot double-pay ...
 * but they fail noisily at commit rather than waiting quietly for a row lock,
 * and a noisy failure that the system was designed to avoid reads as a defect
 * for as long as it takes somebody to re-derive this paragraph."
 *
 * OpenWagerForCashOut deliberately does NOT take it. See its own comment.
 *
 * REDELIVERY IS ROUTINE, NOT AN ERROR. CLAUDE.md section 3 puts Kafka at
 * at-least-once and the results feed's boundary is deliberately inclusive
 * (ResultsSource.Since), so `settle` WILL be handed the same result twice as a
 * matter of course. A zero row count therefore means "already applied", which is
 * a SUCCESSFUL outcome for the consumer: it must not retry, must not log an
 * error, and must not write a second ledger movement. It is the same reading
 * 00006's triggers take -- they permit s -> s and keep the ORIGINAL graded_at,
 * "so a redelivered settlement event cannot move the recorded grading time".
 *
 * NEVER RETRY A FAILED TRANSACTION. postgres.IsTransientConnectError gates
 * retries and covers CONNECTION failures only. A retried settlement replays the
 * ledger writes that accompanied it, and a double-applied payout in a
 * double-entry ledger is not detectable afterwards -- both movements balance.
 *
 * THE LEDGER WRITES ARE NOT HERE. Settling a wager moves money, and the money
 * moves through betting.sql's InsertLedgerTransaction / InsertLedgerEntry, in
 * the SAME transaction as the UPDATE below. There is no settlement-specific
 * ledger statement, because there is no settlement-specific ledger: 00006 makes
 * ledger_entries append-only by trigger, so there is exactly one way money is
 * ever recorded and settlement uses it.
 * ============================================================================
 */


-- THE RESULTS FEED. Events that reached a terminal state, since a watermark.
--
-- CLAUDE.md phase 8: "`settle` consumes a results feed, grades open wagers,
-- writes ledger entries, emits settlement events." This statement is that feed,
-- and it is worth being precise about where its data comes from, because
-- "settlement needs results" is exactly the point at which a project reaches for
-- a fixture file.
--
-- IT IS NOT FABRICATED DATA. internal/ingest/provider/synthetic runs a
-- stochastic market maker that advances each event through scheduled -> live ->
-- ended and carries a domain.Score derived from THE SAME latent process that
-- prices the market (model.go, newEventState/scoreAt) -- the score and the line
-- are two views of one simulated game, not two independent inventions.
-- internal/ingest/writer persists that status and those scores to `events`
-- through its upsert. So this query reads a live generator's own output out of
-- the pipeline's own storage, which is the same path a real provider's results
-- adapter will write down when one is chosen (CLAUDE.md section 13, open
-- decision 1). internal/settlement declares a ResultsSource interface and this
-- is the shipped implementation behind it; the real adapter drops in beside it.
--
-- THREE STATUSES, NOT ONE, and the phase brief's working name for this query
-- ("ListEndedEventsWithScores") described only the first of them. The set is
-- domain's, read off settlement.Result:
--
--   ended      played to a final score. The ordinary case.
--   settled    played, scored, and already marked settled upstream. Included
--              because a redelivered or replayed result must find the same rows
--              it found the first time, and an event that this consumer graded
--              and that the catalogue then advanced to 'settled' would otherwise
--              vanish from the feed mid-batch.
--   cancelled  will never produce a result. Every leg on it voids and every
--              stake comes back, so it IS a result even though it has no score.
--
-- AND `postponed` IS DELIBERATELY ABSENT. domain.EventStatus admits
-- `postponed -> scheduled`, so a postponed event is one that will be played
-- later; voiding its wagers would cancel bets on a game that is still going to
-- happen. settlement/ports.go states the same exclusion on Result.Status.
--
-- THE SCORE IS NOT FILTERED HERE, and that is the interesting decision. A
-- cancelled event legitimately has none, and an 'ended' row with no score is
-- STORABLE -- the catalogue migration constrains the PAIR
-- (events_score_all_or_nothing) and deliberately does not require a score for a
-- started status. So the query returns what the table says and
-- settlement.Result.Validate refuses the junk at the boundary, where the refusal
-- can be counted and logged. Filtering here instead would make a half-scored
-- ended event silently disappear from a feed whose whole job is to be complete,
-- which is the failure mode that pays nobody and reports nothing.
--
-- THE WATERMARK IS `observed_at`, IT IS MONOTONE, AND THE BOUND IS INCLUSIVE.
-- The events upsert only applies a payload `WHERE excluded.observed_at >=
-- events.observed_at`, so the column never moves backwards for a given event.
-- `created_at` and `updated_at` would both be wrong here -- they are
-- database-clock bookkeeping and a Kafka replay restamps them.
--
-- The bound is `>=` and not `>` because a provider poll finalises a WHOLE SLATE
-- at one observation instant, so ties at the boundary are the common case rather
-- than the rare one. Settlement advances its cursor to the last result of a
-- fully-processed batch; an exclusive bound would drop every other result
-- sharing that instant. The cost is that the last batch's final instant is
-- re-read on every poll, which is absorbed by grading being idempotent --
-- re-grading an already-graded leg costs one UPDATE that matches zero rows,
-- whereas losing a result costs a customer their stake.
--
-- Ordered by (observed_at, id) so the ordering is TOTAL and a cursor minted from
-- the last row is unambiguous.
--
-- ON THE PLAN. This is a sequential scan of `events`, and it is the only
-- statement in the generated surface that knowingly is one. No index serves it:
-- events_open_start_idx and events_league_board_idx are both partial on
-- `status IN ('scheduled','live','suspended')` and therefore exclude every row
-- this query wants, which is correct for their purpose and useless for this one.
--
-- It is accepted rather than fixed, for now, on three grounds. `events` grows
-- with the SEASON and not with traffic -- a broad slate is tens of thousands of
-- rows a year, not the hypertable's millions a week. This runs once per poll
-- interval in one background consumer, never per request and never per customer.
-- And the index that would fix it -- `ON events (observed_at) WHERE status IN
-- ('ended','settled','cancelled')` -- is a real cost on the ingest write path,
-- which upserts this table continuously. Migration 00006 records the same kind
-- of decision under "DELIBERATELY ABSENT": the fix is a separate, reviewed
-- migration with a measurement attached, not a speculative index added by
-- whoever wrote the query. `make query-plans` allow-lists relations, so `events`
-- has to be named there for as long as this stands.
--
-- name: ListFinalisedEventsSince :many
SELECT id,
       status,
       score_home,
       score_away,
       observed_at
  FROM events
 WHERE status IN ('ended', 'settled', 'cancelled')
   AND observed_at >= @since
 ORDER BY observed_at, id
 LIMIT @row_limit;


-- Every ungraded leg on one event, with everything the grader needs to decide it.
--
-- This is settlement's unit of work: settlement.LegRef, one row at a time.
-- betting.sql's GetOpenWagerIDsForEvent answers "which tickets does this event
-- touch", which is the operator's question; this answers "which legs on this
-- event still need a result", which is the grader's.
--
-- Both status predicates are needed and they ask different questions -- see
-- GetOpenWagerIDsForEvent's comment for the parlay case where a leg is still
-- pending on a ticket that is already terminal, and grading it would be work
-- with no effect on anything.
--
-- THE THREE GRADING INPUTS, and why each is computed here rather than in Go:
--
--   market_type, role   copied onto the leg at placement and pinned to the
--                       market by a composite FK. leg.go: "Grading a leg must
--                       not require re-reading the Market it came from, because
--                       that read is precisely the thing this type exists to
--                       make impossible."
--
--   grading_line        domain.Leg.GradingLine() spelled in SQL:
--                       COALESCE(teased_line, price_line). It is already the
--                       teased number where one exists and already inverted for
--                       an away spread (domain.EffectiveLine did that at
--                       placement), so the grader applies it as given. Reading
--                       price_line directly would grade a teaser at the line the
--                       market quoted rather than at the one the customer
--                       bought, which is the whole value of a teaser and
--                       therefore the whole error.
--
--   draw_quoted         WHETHER THIS MONEYLINE MARKET ALSO QUOTES A DRAW, and
--                       it is the one grading input a leg CANNOT answer for
--                       itself. On a two-way moneyline a tie is a PUSH and the
--                       stake comes back; on a three-way one it is a LOSS for
--                       both sides and the draw selection wins. That fact is a
--                       property of the MARKET, and domain.Leg deliberately does
--                       not copy it -- so it has to arrive with the pending-leg
--                       list or the grader needs a catalogue query per leg. The
--                       synthetic provider quotes three-way moneylines for the
--                       leagues whose sport admits a draw
--                       (internal/ingest/provider/synthetic/markets.go), so both
--                       shapes are live in this system and neither may be
--                       assumed.
--
-- The EXISTS is a semi-join against selections_market_idx (market_id), which
-- stops at the first matching row, and it is false and ignored for every market
-- type other than moneyline.
--
-- Served by legs_event_status_idx (event_id, status) -- one leading equality and
-- the status as the second column -- joined to wagers by primary key. Ordered by
-- (wager_id, selection_id), which is legs_wager_selection_key's own order, so
-- the caller groups legs under their ticket in one pass without a sort. `limit`
-- is required and bounds the batch: settlement holds the result in memory and an
-- event with an unbounded number of legs on it would otherwise size the poll.
--
-- name: ListPendingLegsForEvent :many
SELECT l.id,
       l.wager_id,
       l.event_id,
       l.market_id,
       l.market_type,
       l.selection_id,
       l.role,
       coalesce(l.teased_line, l.price_line) AS grading_line,
       EXISTS (
           SELECT 1
             FROM selections s
            WHERE s.market_id = l.market_id
              AND s.role = 'draw'
       )::BOOLEAN AS draw_quoted
  FROM legs   l
  JOIN wagers w ON w.id = l.wager_id
 WHERE l.event_id = @event_id
   AND l.status = 'pending'
   AND w.status IN ('placed', 'open')
 ORDER BY l.wager_id, l.selection_id
 LIMIT @row_limit;


-- One ticket, LOCKED, for the transaction that is about to settle it.
--
-- betting.sql's GetWager is the same projection without the lock, for the two
-- read paths that write nothing. This one is taken by settlement, whose very
-- next statements are an UPDATE to this row and a ledger movement about it.
--
-- WHAT THE LOCK BUYS, precisely -- because it is NOT what stops a double
-- payout. SettleWager's guard and 00006's write-once returned amount already do
-- that, in that order, and neither depends on this. What the lock removes is the
-- NOISE: two settle replicas grading two different events of one parlay would
-- otherwise both read, both compute, both write, and one would lose at COMMIT
-- with a trigger exception. With the lock the second one waits, then reads the
-- settled row, then finds nothing to do. settlement/ports.go: "a noisy failure
-- that the system was designed to avoid reads as a defect for as long as it
-- takes somebody to re-derive this paragraph."
--
-- LOCK ORDER, stated because a second lock is what turns waiting into
-- deadlocking. A settlement transaction takes exactly one row lock, here, on the
-- wager. It does not lock the legs (their guarded UPDATE takes its own row lock
-- when it runs, always after this one), does not lock `users`, and does not lock
-- ledger rows (they are inserts). A placement transaction takes exactly one row
-- lock too, on `users`, and never touches a wager that exists. The two paths
-- therefore have no lock in common and no cycle is constructible between them.
-- Anything added here that locks a second relation must justify the order it
-- takes them in.
--
-- Returns no row when the ticket does not exist, which the caller reports as
-- ErrWagerNotFound. That really is exceptional on this path: the identifier came
-- from a leg row the same transaction just read, and legs.wager_id is a foreign
-- key.
--
-- The legs come from betting.sql's ListWagerLegs, unlocked, in the same
-- transaction. They do not need their own lock: each is guarded by its own
-- UPDATE, and a leg cannot move while this transaction holds its wager because
-- every writer of a leg reaches it through the wager first.
--
-- name: GetWagerForSettlement :one
SELECT id, user_id, kind, status,
       stake_minor, accepted_decimal, rounding,
       potential_payout_minor, potential_profit_minor,
       teaser_points, round_robin_id,
       returned_minor, net_return_minor,
       placed_at, transitioned_at
  FROM wagers
 WHERE id = @id
   FOR UPDATE;


-- Grade one leg. `:execrows` -- THE CALLER MUST CHECK THE COUNT.
--
-- `AND status = 'pending'` is the guard, and it is what makes a redelivered
-- grading a no-op instead of a corruption. legs_assert_transition would refuse
-- an attempt to re-grade a terminal leg with SQLSTATE 23001 anyway; the guard
-- turns that exception into a zero row count, which is the right shape because
-- redelivery is ROUTINE and an exception is for something that went wrong.
-- Zero maps to settlement.ErrLegAlreadyGraded, which the caller treats as
-- "somebody else applied this" and NOT as success.
--
-- graded_at is a DOMAIN INSTANT, passed explicitly, never now(). Leg.WithStatus
-- takes the instant as a parameter for exactly this reason: a settlement
-- replayed from Kafka must re-apply the ORIGINAL instant, and it is the
-- PROVIDER's observation instant for the result (events.observed_at) rather than
-- the settle service's clock, so two runs over the same result stamp the same
-- time. legs_assert_transition makes graded_at write-once, so a second attempt
-- at a different instant is refused rather than silently moving the record.
--
-- `updated_at` is NOT set here. legs_set_updated_at stamps it from the server
-- clock on every UPDATE, and 00006 draws the line between the two hard: a
-- trigger may stamp updated_at, no trigger may write a column carrying a domain
-- instant. Setting it in this statement would be the application reaching across
-- that line from the other side.
--
-- The status value is a parameter and not four statements: legs_status_defined
-- constrains it to pending | won | lost | void | push, and
-- legs_graded_at_iff_graded makes "terminal implies graded_at present"
-- structural, so a caller that passed 'pending' with a non-null graded_at is
-- refused by the database rather than by a missing branch here.
--
-- name: GradeLeg :execrows
UPDATE legs
   SET status    = @status,
       graded_at = @graded_at
 WHERE id = @id
   AND status = 'pending';


-- Move a ticket from 'placed' to 'open' -- its first event has gone live.
--
-- Wager.Open(at) is the domain transition. WagerStatus.CanTransitionTo permits
-- placed -> open and nothing else into 'open', and wagers_assert_transition
-- enforces the same edge, so the guard here is `status = 'placed'` and a ticket
-- that is already open (or already settled) returns zero rows.
--
-- This is the one status change that moves no money: the stake is already in
-- escrow from placement and stays there. It exists so that "open position" means
-- something on the history screen and so the exposure metric can separate a
-- ticket on a game in progress from one on a game that has not started.
-- WagerStatus.HoldsEscrow() covers both, which is why wagers_user_open_idx's
-- partial predicate names them together.
--
-- transitioned_at is monotone -- wagers_assert_transition raises on a stale
-- instant, which is Wager.stamp's ErrStaleUpdate in SQL -- so passing a clock
-- reading older than the ticket's last transition is an ERROR and not a silent
-- no-op. Pass the instant the event was observed to go live, not the instant
-- this consumer woke up.
--
-- name: OpenWager :execrows
UPDATE wagers
   SET status          = 'open',
       transitioned_at = @transitioned_at
 WHERE id = @id
   AND status = 'placed';


-- Settle one ticket. `:execrows` -- THE CALLER MUST CHECK THE COUNT.
--
-- The single most consequential statement in phase 8: it is what decides what a
-- customer is paid.
--
-- THE GUARD IS `status IN ('placed', 'open')`, which is
-- WagerStatus.HoldsEscrow() -- exactly the tickets that can still change. A
-- terminal ticket returns zero rows, which is how a redelivered settlement event
-- is absorbed, and which maps to settlement.ErrWagerAlreadySettled.
-- wagers_assert_transition additionally refuses a terminal-to-terminal edit and
-- makes returned_minor write-once, so even a caller that ignored the row count
-- could not re-price a settled ticket; the guard is what turns that refusal into
-- an ordinary, expected zero.
--
-- BOTH MONEY COLUMNS ARE PASSED, NOT DERIVED. net_return_minor is
-- returned_minor - stake_minor and the schema asserts that identity
-- (wagers_net_return_identity), so a mismatched pair is refused. They are both
-- parameters anyway because domain.Wager computes them together in settleAt
-- under the ticket's OWN Rounding -- money.go: "a silent default is how a house
-- edge appears in a ledger that nobody meant to put one in" -- and a SQL
-- expression `returned_minor - stake_minor` here would be a second
-- implementation of arithmetic the domain already did. The CHECK is the
-- reconciliation between the two; it is not a substitute for either.
--
-- WHAT THE DATABASE WILL REFUSE, so the caller can recognise it rather than
-- reporting a generic failure. wagers_return_matches_outcome states the return
-- rule per outcome as a CHECK: 'lost' must return exactly 0, 'void' and 'push'
-- must return exactly the stake, 'won' must return at least the stake and at
-- most the potential payout, 'cashed_out' must return something strictly
-- positive and at most the potential payout. A violation is SQLSTATE 23514 and
-- it means the grading produced a number the ticket cannot pay -- wager.go: "a
-- return above the maximum is an arithmetic fault caught here rather than an
-- overpayment discovered in a reconciliation."
--
-- THE LEDGER MOVEMENT IS A SEPARATE WRITE IN THE SAME TRANSACTION. This
-- statement changes what the ticket SAYS it returned; betting.sql's
-- InsertLedgerTransaction / InsertLedgerEntry are what actually move the money,
-- and the ledger's deferred balance trigger judges them at COMMIT. Running this
-- UPDATE without those entries produces a settled ticket that paid nobody, and
-- nothing in the schema catches it -- the two are held together by being in one
-- postgres.InTx call and by nothing else, which is why phase 8's rule is that
-- every path writing the ledger goes through InTx.
--
-- name: SettleWager :execrows
UPDATE wagers
   SET status           = @status,
       returned_minor   = @returned_minor,
       net_return_minor = @net_return_minor,
       transitioned_at  = @transitioned_at
 WHERE id = @id
   AND status IN ('placed', 'open');


-- The placement instant of the oldest ticket still holding escrow.
--
-- It seeds the results cursor at startup, and it is why settlement needs no
-- cursor table. A ticket cannot be waiting on a result that was already final
-- when the ticket was written, so the earliest open placement is a sound lower
-- bound on "the oldest result settlement could still care about". Everything
-- older has either been graded or was never bet on, and re-reading it would be
-- work with no possible outcome.
--
-- The consumer subtracts a lookback from the answer before using it, because the
-- two instants come from different clocks -- the placement instant is this
-- system's, the result instant is the provider's -- so a bound that is exactly
-- right in principle is off by whatever skew exists between them. That is
-- settlement's decision and not this query's; see Store.OldestUnsettledAt.
--
-- `ORDER BY placed_at LIMIT 1` RATHER THAN `min(placed_at)`, and the difference
-- is not stylistic. An aggregate over an empty set returns one row containing
-- NULL, which sqlc types from the column and would scan into a non-pointer
-- time.Time -- so "no open wagers", the state of every fresh database, would
-- fail at the scan. This form returns NO ROW instead, which is pgx.ErrNoRows and
-- maps cleanly onto the (at, found, err) triple the port asks for.
--
-- The predicate is WagerStatus.HoldsEscrow() and matches wagers_user_open_idx's
-- partial predicate exactly, so the planner can walk that index -- which
-- migration 00006 sized for precisely this shape: "settled tickets accumulate
-- without bound and are never an open position, so this index stays small
-- forever while the table does not." It leads with user_id and so cannot supply
-- the ordering, but it bounds the set to the open tickets before the sort, which
-- is the part that matters.
--
-- name: GetOldestUnsettledPlacedAt :one
SELECT id, placed_at
  FROM wagers
 WHERE status IN ('placed', 'open')
 ORDER BY placed_at
 LIMIT 1;


-- The ticket a customer is asking to cash out, if it is still cashable.
--
-- CLAUDE.md section 6 (Betting): "cash-out pricing on live events."
--
-- Three predicates, and each refuses a different request:
--
--   id      = @id            the ticket asked for
--   user_id = @user_id       IT IS THEIRS. Scoped here rather than compared in
--                            Go, because this is a WRITE authorisation and not a
--                            read: an unscoped read followed by an ownership
--                            check in the caller is a check-then-act, and the
--                            version of that bug which ships is the one where a
--                            later refactor moves the check. A wrong user gets
--                            no row, which the caller reports as 404 -- not 403,
--                            which would confirm the ticket exists and turn this
--                            into an enumeration oracle over other customers'
--                            wager ids.
--   status IN ('placed','open')
--                            it is not already terminal. A settled ticket has
--                            nothing left to buy back.
--
-- NO `FOR UPDATE`, unlike GetWagerForSettlement above, and the asymmetry is
-- deliberate. A settlement transaction is going to settle; waiting for the row
-- is strictly better than racing for it. A cash-out transaction MIGHT settle --
-- most of them are quotes that end in the customer declining -- and holding a
-- row lock across a pricing read that reaches the prices hypertable would block
-- settlement on a decision nobody has made yet. If the customer does accept and
-- a result lands first, SettleWager's guard returns zero rows, the transaction
-- rolls back with nothing written, and the customer is told the ticket settled.
-- That is the correct answer and it costs one wasted transaction.
--
-- WHAT THIS DOES NOT RETURN, and must not. It returns the ticket, not a price.
-- The cash-out value is computed in Go as
--
--     round(stake x remainingFairDecimal x (1 - DefaultCashOutMarginBps/10000))
--
-- where remainingFairDecimal is the product of the FAIR (devigged, from the
-- sharp reference book -- ADR 0006) decimal prices of the still-pending legs
-- times the graded multiplier of the decided ones. The fair price is used and a
-- NAMED haircut is then subtracted, rather than quoting off the offered price,
-- so that "what did the book charge me to close early" has an answer: quoting
-- off the offered price hides the same take inside the vig and makes the
-- question unanswerable. None of that arithmetic belongs in SQL -- it is
-- floating-point odds work that internal/domain/odds owns and tests
-- exhaustively, and a second implementation here would eventually disagree with
-- it about somebody's money.
--
-- The legs come from betting.sql's ListWagerLegs and the current fair prices
-- from internal/pricing through betting.FairPrices, whose FairPrice.ObservedAt
-- is the underlying QUOTE's instant rather than the instant the fair value was
-- computed -- which is what makes the staleness refusal real. A remaining leg
-- whose reference quote predates the window is history rather than a line, and
-- the cash-out is refused rather than priced off it.
--
-- name: OpenWagerForCashOut :one
SELECT id, user_id, kind, status,
       stake_minor, accepted_decimal, rounding,
       potential_payout_minor, potential_profit_minor,
       teaser_points, round_robin_id,
       returned_minor, net_return_minor,
       placed_at, transitioned_at
  FROM wagers
 WHERE id = @id
   AND user_id = @user_id
   AND status IN ('placed', 'open');


-- Every ticket of one round robin, so the parent can be reported as a whole.
--
-- A round robin is N independent tickets sharing a parent row (00006:
-- "A '3-team round robin by 2s' is not one bet: it is three independent two-leg
-- parlays -- AB, AC, BC -- each of which wins, loses, and settles on its own").
-- Each settles through SettleWager above, on its own, at its own time. This is
-- the read that lets the history screen group them under their parent and answer
-- "how did the round robin do" as a fold over the tickets rather than as a
-- second stored total -- RoundRobin.TotalStake() is derived for the same reason.
--
-- Served by wagers_round_robin_idx, which is partial on `round_robin_id IS NOT
-- NULL` because a straight has no parent and would only bloat it. Ordered by id
-- so the combination order is stable between renders.
--
-- name: ListWagersInRoundRobin :many
SELECT id, user_id, kind, status,
       stake_minor, accepted_decimal, rounding,
       potential_payout_minor, potential_profit_minor,
       teaser_points, round_robin_id,
       returned_minor, net_return_minor,
       placed_at, transitioned_at
  FROM wagers
 WHERE round_robin_id = @round_robin_id
 ORDER BY id;
