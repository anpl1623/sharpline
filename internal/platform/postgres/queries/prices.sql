/*
 * ============================================================================
 * prices -- the hypertable, and the hottest read in the system
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumers: `ingest`'s writer (CLAUDE.md phase 3), `api` (phase 5) and the
 * board's line-movement chart (phase 7).
 *
 * All three queries are served by ONE index -- prices_natural_key_idx
 * (selection_id, book_id, observed_at DESC) -- which is simultaneously the
 * hypertable's uniqueness constraint on domain.Price's natural key. Migration
 * 00003 has the full argument for why it is a UNIQUE INDEX with a descending third
 * column rather than a PRIMARY KEY; the short version is that the descending order
 * is what lets the latest-price and history reads run with no sort node, and
 * PRIMARY KEY syntax cannot express DESC.
 *
 * THE TIME BOUND ON A READ IS NOT OPTIONAL. `prices` is partitioned on observed_at
 * with 12-hour chunks and migration 00004 deliberately installs NO retention
 * policy, so the chunk count grows for the life of the deployment. A read without
 * a lower bound on observed_at defeats chunk exclusion and makes the planner
 * consult the index on every chunk that has ever existed. Both read queries below
 * therefore take their bounds as REQUIRED parameters -- there is no unbounded
 * variant to reach for by accident.
 *
 * WHERE THIS SITS IN THE ARCHITECTURE. CLAUDE.md section 3 puts the current-line
 * snapshot in Redis and in the compacted `odds.normalized` topic. Postgres is the
 * SOURCE OF TRUTH, not the board's hot path: LatestPriceForEachBookOnSelections is
 * what a cold start or a cache miss falls back to. Redis is "never the source of
 * truth", and this is what it is never the source of truth *for*.
 * ============================================================================
 */


-- The current line for each (selection, book) pair, for a set of selections.
--
-- THE HOTTEST QUERY IN THE SYSTEM. Renders every price cell on the board.
--
-- DISTINCT ON (selection_id, book_id) with ORDER BY selection_id, book_id,
-- observed_at DESC asks for exactly the pathkeys prices_natural_key_idx provides,
-- so this is a bounded index scan that stops at the first row of each group. Those
-- pathkeys are neither the forward nor the reverse of an all-ascending index,
-- which is why that index's third column is DESC.
--
-- `observed_after` is REQUIRED and is a staleness horizon as well as a chunk
-- filter: a quote older than the horizon is not a current line, it is history. The
-- board passes something on the order of an hour; a cache-fill pass passes the
-- same window it is filling.
--
-- ingested_at is returned alongside observed_at because (ingested_at -
-- observed_at) is the provider-attributable half of the staleness SLO and the API
-- should be able to report it without a second query.
--
-- name: LatestPriceForEachBookOnSelections :many
SELECT DISTINCT ON (selection_id, book_id)
       selection_id,
       book_id,
       decimal_odds,
       line,
       observed_at,
       ingested_at
  FROM prices
 WHERE selection_id = ANY(@selection_ids::TEXT[])
   AND observed_at > @observed_after
 ORDER BY selection_id, book_id, observed_at DESC;


-- Every quote one book made on one selection inside a window, oldest first --
-- the line-movement chart (CLAUDE.md section 6 Core: "line movement charts from
-- history").
--
-- Two leading equalities then a range on the third column: one contiguous index
-- range scan. ORDER BY observed_at ASC against a DESC index costs nothing --
-- Postgres scans a b-tree backwards as cheaply as forwards -- so there is still
-- no sort node.
--
-- The window is half-open [from_inclusive, to_exclusive) so that adjacent windows
-- tile without double-counting the boundary quote.
--
-- name: ListPriceHistoryForSelectionAtBook :many
SELECT selection_id,
       book_id,
       decimal_odds,
       line,
       observed_at,
       ingested_at
  FROM prices
 WHERE selection_id = @selection_id
   AND book_id = @book_id
   AND observed_at >= @from_inclusive
   AND observed_at <  @to_exclusive
 ORDER BY observed_at;


-- Record one observed quote. The only write path into the hypertable.
--
-- APPEND-ONLY BY TRIGGER: migration 00003 installs prices_no_update,
-- prices_no_delete and prices_no_truncate, so there is no update statement to
-- write and no ON CONFLICT DO UPDATE to reach for. A new price is a new row
-- (CLAUDE.md section 4).
--
-- ON CONFLICT DO NOTHING is the idempotency guard, and it is load-bearing rather
-- than defensive: `ingest` consumes from Kafka, delivery is at-least-once, and a
-- consumer-group rebalance or a deliberate topic replay will redeliver a record
-- whose (selection_id, book_id, observed_at) is already stored. Without this,
-- replay is a unique-violation storm; with it, replay is a no-op.
--
-- The conflict target is stated explicitly rather than left bare so that the
-- statement asserts WHICH uniqueness it tolerates -- a future second unique index
-- on this table would then surface as an error rather than being silently
-- swallowed. Index inference matches on the key columns and their opclasses and
-- ignores ASC/DESC, so naming the columns in ascending order matches the
-- descending prices_natural_key_idx. (Verified against Postgres 17 + TimescaleDB:
-- the insert plans as a Custom Scan (HypertableModify) with ON CONFLICT arbiter
-- prices_natural_key_idx.)
--
-- observed_at is the PROVIDER's own instant, propagated unchanged; ingested_at is
-- when ingest received the payload. Neither is defaulted here -- created_at is the
-- only clock the database supplies -- because a replay must reproduce the original
-- event times, not restamp them.
--
-- This is `:exec` rather than `:copyfrom` or `:batchexec` on purpose. COPY cannot
-- express ON CONFLICT, which is the whole point of the statement, and the write
-- rate migration 00003 sizes for is ~2.7M rows/day (about 31/s mean). A caller
-- that needs a burst of these in one round trip can hand them to pgx.Batch
-- without any change here; promoting this to `:batchexec` is a one-word edit if
-- measurement ever demands it.
--
-- name: InsertPrice :exec
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES (@selection_id, @book_id, @decimal_odds, @line, @observed_at, @ingested_at)
ON CONFLICT (selection_id, book_id, observed_at) DO NOTHING;
