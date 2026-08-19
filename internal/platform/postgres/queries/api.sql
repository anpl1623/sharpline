/*
 * ============================================================================
 * api -- the reads the REST surface needs that catalogue.sql and prices.sql
 *        deliberately do not answer
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumer: `api` only (CLAUDE.md phase 5), through internal/httpapi/pgstore.
 *
 * WHY A FOURTH QUERY FILE INSTEAD OF EDITING catalogue.sql
 * --------------------------------------------------------
 * catalogue.sql's queries have TWO consumers each -- the API and `ingest`'s
 * adaptive-polling scheduler, which asks ListOpenEventsStartingBefore on every
 * scheduling tick and is the highest-frequency read in the system. Widening
 * those queries with pagination parameters the scheduler does not want would
 * make the hottest read in the system carry the API's concerns. The three
 * board/search queries below are the SAME questions with a keyset predicate
 * bolted on, and they are the API's alone.
 *
 * -----------------------------------------------------------------------------
 * WHY EVERY LIST HERE COMES IN A FIRST-PAGE / AFTER-CURSOR PAIR
 * -----------------------------------------------------------------------------
 * The API paginates by KEYSET, never by OFFSET. `ingest` writes `events`
 * continuously, and OFFSET re-evaluates the whole ordered set on every page: a
 * row inserted ahead of the offset between page N and page N+1 pushes one row
 * across the boundary and the reader NEVER SEES IT, and a row that leaves the
 * set duplicates one. On a board that changes every few seconds that is the
 * normal case, not a rare race.
 *
 * A keyset predicate names the last row instead of counting rows. It needs two
 * things that catalogue.sql's board queries do not provide:
 *
 *   1. A TOTAL ordering. `ORDER BY scheduled_start` alone is not total -- two
 *      fixtures kicking off at the same instant are returned in an arbitrary
 *      order that may differ between two executions of the same query, so a
 *      cursor pointing "after" one of them cannot say which. Every ordering
 *      below is `(scheduled_start, id)`, and `id` is a primary key, so the
 *      ordering is total and the cursor is unambiguous.
 *
 *   2. A LOWER bound on that ordering. catalogue.sql's board queries take an
 *      upper bound (`scheduled_start < starting_before`) because that is what a
 *      polling horizon is. A cursor is the opposite direction and cannot be
 *      expressed by narrowing the horizon.
 *
 * The pair exists rather than one query with a nullable cursor because
 * `(@after_start IS NULL OR (scheduled_start, id) > (@after_start, @after_id))`
 * is not sargable -- the OR defeats the index the whole design depends on. Two
 * statements, each with a predicate the planner can use, beats one statement
 * that is correct and slow.
 *
 * The row comparison is written as a ROW-VALUE comparison
 * `(scheduled_start, id) > (@after_start, @after_id)` rather than as the
 * expanded `a > x OR (a = x AND b > y)`. They are logically identical and
 * PostgreSQL plans the row form as a single index range; the expanded form is
 * where off-by-one duplicate-row bugs live.
 *
 * NOTHING HERE WRITES A BALANCE. There is no balance column to write
 * (CLAUDE.md section 4) -- see GetUserCashAndEscrowBalances, which folds
 * ledger_entries through the account_balances view.
 * ============================================================================
 */


-- ============================================================================
-- Board: the cross-league odds board, first page
-- ============================================================================
--
-- The same question as catalogue.sql's ListOpenEventsStartingBefore, with the
-- ordering made TOTAL so a cursor minted from the last row is unambiguous. The
-- status literals are written out rather than parameterised so they match the
-- predicate of the partial index events_open_start_idx exactly; passing the set
-- as a parameter would silently lose the index.
--
-- The caller asks for one row more than the page size and reports has_more from
-- whether it got it. That is one extra row rather than a second COUNT(*) query,
-- and a count over a continuously-written set is stale before it is serialised.
--
-- name: ListBoardEventsFirstPage :many
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < @starting_before
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- The cross-league board, continued from a cursor.
--
-- `starting_before` is repeated from the first page and MUST be the same value:
-- it is the window the cursor was minted inside, and changing it between pages
-- would silently change the set being paged. internal/httpapi binds it into the
-- cursor and rejects a cursor presented against a different window rather than
-- returning a page from a set the caller did not ask for.
--
-- name: ListBoardEventsAfterCursor :many
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < @starting_before
   AND (scheduled_start, id) > (@after_start, @after_id::TEXT)
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- ============================================================================
-- Board: one league
-- ============================================================================
--
-- Served by events_league_board_idx (league_id, scheduled_start) WHERE status IN
-- (...). A cross-league query cannot use an index led by league_id, which is why
-- this is a separate statement rather than a nullable league filter on the one
-- above.
--
-- name: ListLeagueBoardEventsFirstPage :many
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE league_id = @league_id
   AND status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < @starting_before
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- name: ListLeagueBoardEventsAfterCursor :many
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE league_id = @league_id
   AND status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < @starting_before
   AND (scheduled_start, id) > (@after_start, @after_id::TEXT)
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- ============================================================================
-- Search: competitor name prefix, paginated
-- ============================================================================
--
-- catalogue.sql's SearchOpenEventsByCompetitorPrefix with a total ordering and a
-- cursor. Everything that file says about this query still holds and is not
-- repeated here: prefix-only (no pg_trgm), UNION rather than UNION ALL so an
-- event whose two competitors both match appears once, and a status set of
-- ('scheduled','live') that is deliberately NARROWER than the board's because
-- this endpoint answers "what can I bet on".
--
-- The caller escapes `%`, `_` and `\` in the prefix before binding it. PostgreSQL
-- LIKE takes `\` as its escape character by default under
-- standard_conforming_strings, so no ESCAPE clause is needed and the call site
-- keeps the contract catalogue.sql documents.
--
-- ---------------------------------------------------------------------------
-- WHY A BOOLEAN OR HERE WHERE catalogue.sql USES A UNION
-- ---------------------------------------------------------------------------
-- catalogue.sql matches the two competitor columns with `UNION`, and notes that
-- it is UNION rather than UNION ALL so an event whose two competitors both match
-- appears once. An `OR` of the same two predicates gets that de-duplication for
-- free -- a row is a row, it is either in the result or not -- and PostgreSQL
-- plans it as a BitmapOr over the SAME two partial expression indexes, so the
-- access path is unchanged while the union's dedupe sort disappears.
--
-- The reason it is written this way rather than copied is not the plan, though.
-- A keyset predicate CANNOT be expressed against a bare UNION: `ORDER BY` over a
-- union resolves against the union's OUTPUT columns, so a
-- `(scheduled_start, id) > (...)` reference is ambiguous between a branch's
-- relation and the union's output, and wrapping the union in a subquery does not
-- help because the ambiguity survives the flattening. The OR form has one
-- relation, one unambiguous ordering, and one place the cursor is compared.
--
-- name: SearchBoardEventsFirstPage :many
SELECT id, league_id, kind, name,
       home_competitor_name, away_competitor_name,
       scheduled_start, status
  FROM events
 WHERE status IN ('scheduled', 'live')
   AND (
        (home_competitor_name IS NOT NULL
         AND lower(home_competitor_name) LIKE lower(@prefix::TEXT) || '%')
     OR (away_competitor_name IS NOT NULL
         AND lower(away_competitor_name) LIKE lower(@prefix::TEXT) || '%')
   )
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- name: SearchBoardEventsAfterCursor :many
SELECT id, league_id, kind, name,
       home_competitor_name, away_competitor_name,
       scheduled_start, status
  FROM events
 WHERE status IN ('scheduled', 'live')
   AND (
        (home_competitor_name IS NOT NULL
         AND lower(home_competitor_name) LIKE lower(@prefix::TEXT) || '%')
     OR (away_competitor_name IS NOT NULL
         AND lower(away_competitor_name) LIKE lower(@prefix::TEXT) || '%')
   )
   AND (scheduled_start, id) > (@after_start, @after_id::TEXT)
 ORDER BY scheduled_start, id
 LIMIT @row_limit;


-- ============================================================================
-- One market, and one selection, by identifier
-- ============================================================================
--
-- catalogue.sql reaches markets and selections only through their parent
-- (ListMarketsForEvent, ListSelectionsForMarkets), because that is the shape the
-- event detail page and the board want. The multi-book comparison endpoint and
-- the line-history endpoint are addressed BY the market and BY the selection, so
-- they need the primary-key lookup, and they need the parent ids to build the
-- response's breadcrumb without a second round trip.

-- name: GetMarketWithEvent :one
SELECT m.id, m.event_id, m.type, m.line, m.subject, m.status, m.observed_at,
       e.league_id AS event_league_id,
       e.name      AS event_name,
       e.status    AS event_status
  FROM markets m
  JOIN events  e ON e.id = m.event_id
 WHERE m.id = @market_id;


-- Every market on a SET of events -- the board's market tree.
--
-- catalogue.sql's ListMarketsForEvent takes one event, which is right for the
-- detail page and wrong for the board: a page of fifty events would be fifty
-- round trips, and the per-call overhead would dominate a query that is
-- otherwise a bounded index scan. This is the same index (markets_event_type_idx)
-- driven by an ANY(...) over the page's event ids.
--
-- Ordered by (event_id, type, id) so the caller can group the result by event in
-- one pass instead of building a map, and so the tree does not reshuffle between
-- renders.
--
-- name: ListMarketsForEvents :many
SELECT id, event_id, type, line, subject, status, observed_at
  FROM markets
 WHERE event_id = ANY(@event_ids::TEXT[])
 ORDER BY event_id, type, id;


-- name: GetSelectionWithMarket :one
SELECT s.id, s.market_id, s.market_type, s.role, s.name,
       m.event_id AS market_event_id,
       m.line     AS market_line,
       m.status   AS market_status
  FROM selections s
  JOIN markets    m ON m.id = s.market_id
 WHERE s.id = @selection_id;


-- ============================================================================
-- Downsampled line history
-- ============================================================================
--
-- The raw variant is prices.sql's ListPriceHistoryForSelectionAtBook. This is
-- the same window aggregated into fixed-width buckets, because a market quoted
-- every two seconds for a week is ~300k points and no chart draws them.
--
-- WHY open/high/low/close AND NOT A MEAN. The mean of a line is a price nobody
-- traded at. CLV is the placement price against the CLOSING price, and steam
-- detection is a velocity over the endpoints of a window -- both are computed
-- from open and close, and a mean discards exactly those. `samples` is returned
-- so a client can tell a bucket carrying one quote from a bucket carrying two
-- hundred, which is itself the signal that a market went active.
--
-- WHY NOT time_bucket(). TimescaleDB's own bucketing function would be the
-- natural choice and would let the planner exploit chunk boundaries. It is not
-- used because sqlc parses SQL against its own embedded PostgreSQL catalogue and
-- has no knowledge of the timescaledb extension's functions, so a query calling
-- it does not generate. The epoch-floor arithmetic below is standard SQL, is
-- exactly equivalent for the fixed widths the API exposes, and keeps this query
-- inside the same `sqlc diff` drift gate as every other query in this directory.
-- Chunk exclusion is unaffected: it comes from the observed_at predicate, which
-- is still a bounded range on the partitioning column.
--
-- The bounds are REQUIRED, for the reason prices.sql's header gives at length:
-- `prices` grows chunks for the life of the deployment and migration 00004
-- installs no retention policy, so a read with no lower bound consults an index
-- on every chunk that has ever existed. There is no unbounded variant here
-- either.
--
-- name: ListBucketedPriceHistory :many
SELECT to_timestamp(
           floor(extract(epoch FROM observed_at) / @bucket_seconds::DOUBLE PRECISION)
           * @bucket_seconds::DOUBLE PRECISION
       )::TIMESTAMPTZ                                                            AS bucket_start,
       (array_agg(decimal_odds ORDER BY observed_at ASC))[1]::DOUBLE PRECISION   AS open_odds,
       max(decimal_odds)::DOUBLE PRECISION                                       AS high_odds,
       min(decimal_odds)::DOUBLE PRECISION                                       AS low_odds,
       (array_agg(decimal_odds ORDER BY observed_at DESC))[1]::DOUBLE PRECISION  AS close_odds,
       coalesce((array_agg(line ORDER BY observed_at DESC))[1], 0)::DOUBLE PRECISION AS close_line,
       bool_or(line IS NOT NULL)::BOOLEAN                                        AS has_line,
       count(*)::BIGINT                                                          AS samples
  FROM prices
 WHERE selection_id = @selection_id
   AND book_id = @book_id
   AND observed_at >= @from_inclusive
   AND observed_at <  @to_exclusive
 GROUP BY 1
 ORDER BY 1
 LIMIT @row_limit;


-- ============================================================================
-- Account
-- ============================================================================

-- The authenticated user's profile, and whether a CONFIRMED TOTP factor exists.
--
-- The LEFT JOIN reaches user_totp for exactly one bit -- `confirmed_at IS NOT
-- NULL` -- and selects NOTHING else from it. Migration 00005 is explicit that
-- the shared secret is a bearer credential and that the table "is never included
-- in a `SELECT *` join against users, is never returned by any API handler in
-- any shape, and is redacted from logs". Projecting the boolean rather than the
-- row is what makes that structural here: there is no ciphertext column in this
-- result set to leak.
--
-- An UNCONFIRMED enrolment reports false. It is not a second factor -- treating
-- it as one would lock out a user whose QR scan failed.
--
-- name: GetAccountProfile :one
SELECT u.id,
       u.email,
       u.status,
       u.created_at,
       (t.confirmed_at IS NOT NULL)::BOOLEAN AS totp_confirmed,
       (t.user_id IS NOT NULL)::BOOLEAN      AS totp_enrolment_started
  FROM users u
  LEFT JOIN user_totp t ON t.user_id = u.id
 WHERE u.id = @user_id;


-- Both of a user's derived balances in one round trip.
--
-- THE BALANCE IS A FOLD OVER ledger_entries, through the account_balances view.
-- There is no balance column anywhere in the schema and migration 00006 makes
-- that structural rather than conventional: a stored balance can be stale, and a
-- bet slip validated against a stale balance is an overdraft.
--
-- Returns at most two rows, one per account kind, and returns NO ROW for an
-- account that has never moved. That absence is meaningful and the caller must
-- render it as zero rather than as an error: "touched and nets to nothing" and
-- "never touched" are different facts, and entry_count is what distinguishes
-- them.
--
-- If this fold ever becomes too slow the answer is a MATERIALISED VIEW refreshed
-- from the entries, never a mutable column: a materialised view can be provably
-- rebuilt from the ledger, a column cannot.
--
-- name: GetUserCashAndEscrowBalances :many
SELECT account_kind,
       balance_minor,
       entry_count
  FROM account_balances
 WHERE account_user_id = @user_id
   AND account_kind IN ('user_cash', 'user_escrow');


-- ============================================================================
-- Self-imposed limits (responsible gaming)
-- ============================================================================
--
-- The current limit for a (user, kind, period) is the row with superseded_at
-- IS NULL, which is exactly the predicate of the partial unique index
-- user_limits_current_key -- so "at most one current limit per kind and period"
-- is a database guarantee and this query cannot return a duplicate pair.
--
-- WHY amount_minor AND duration_seconds ARRIVE AS A VALUE PLUS A FLAG
--
-- Both columns are nullable BY DESIGN: a money limit has no duration and a
-- session limit has no amount, and migration 00005's three biconditionals make
-- any other combination unstorable. sqlc's override table maps amount_minor to
-- domain.Money and duration_seconds to int32 regardless of nullability, so a
-- NULL would be scanned into a non-pointer integer and fail at runtime -- on the
-- session-limit row specifically, which is the one a money-limit test never
-- reaches.
--
-- So the value is coalesced to zero and its PRESENCE is projected separately.
-- The caller reconstructs the optional from the pair. Zero is a safe sentinel
-- only because it is unstorable: the CHECK constraints require amount_minor > 0
-- and duration_seconds > 0, so a zero here can only mean NULL.
--
-- name: ListCurrentUserLimits :many
SELECT id, user_id, kind, period,
       coalesce(amount_minor, 0)::BIGINT       AS amount_minor,
       (amount_minor IS NOT NULL)::BOOLEAN     AS has_amount,
       coalesce(duration_seconds, 0)::INTEGER  AS duration_seconds,
       (duration_seconds IS NOT NULL)::BOOLEAN AS has_duration,
       requested_at, effective_from
  FROM user_limits
 WHERE user_id = @user_id
   AND superseded_at IS NULL
 ORDER BY kind, period;


-- The one current limit for a (user, kind, period), or no row.
--
-- Read inside the same transaction that supersedes it. It is the "before" state
-- the audit entry records, and it is what decides whether the new value is a
-- TIGHTENING (effective immediately) or a LOOSENING (effective after the
-- cooling-off period) -- a decision that cannot be made without knowing what is
-- currently in force.
--
-- name: GetCurrentUserLimit :one
SELECT id, user_id, kind, period,
       coalesce(amount_minor, 0)::BIGINT       AS amount_minor,
       (amount_minor IS NOT NULL)::BOOLEAN     AS has_amount,
       coalesce(duration_seconds, 0)::INTEGER  AS duration_seconds,
       (duration_seconds IS NOT NULL)::BOOLEAN AS has_duration,
       requested_at, effective_from
  FROM user_limits
 WHERE user_id = @user_id
   AND kind = @kind
   AND period = @period
   AND superseded_at IS NULL;


-- Close the current limit so a new one can replace it.
--
-- :execrows, and the caller MUST check the count. The row is append-only
-- (user_limits_append_only refuses an edit of the substantive columns and refuses
-- a DELETE outright), superseded_at is write-once, and the partial unique index
-- permits exactly one open row per (user, kind, period). So a zero row count
-- means another request superseded the same row first, and the transaction must
-- abort with a conflict rather than insert a second current limit -- which the
-- unique index would refuse anyway, one statement later and with a worse error.
--
-- name: SupersedeUserLimit :execrows
UPDATE user_limits
   SET superseded_at = @superseded_at
 WHERE id = @id
   AND superseded_at IS NULL;


-- Record a new limit. Append-only: this never edits the row it replaces.
--
-- The database refuses every impossible combination through three
-- biconditionals (a session limit denominated in money, a money limit
-- denominated in seconds, a 'session' period on a money kind), so a bug in the
-- API's validation surfaces as a check violation rather than as a stored limit
-- that can never fire.
--
-- name: InsertUserLimit :exec
INSERT INTO user_limits (id, user_id, kind, period, amount_minor,
                         duration_seconds, requested_at, effective_from)
VALUES (@id, @user_id, @kind, @period, sqlc.narg(amount_minor)::BIGINT,
        sqlc.narg(duration_seconds)::INTEGER, @requested_at, @effective_from);


-- ============================================================================
-- Audit log
-- ============================================================================
--
-- CLAUDE.md section 6 (Platform): "audit log on every state-changing action".
-- Every write path in internal/httpapi calls this INSIDE the transaction that
-- performs the change, so an action and its audit record commit together or
-- neither does. An audit log written after the commit is one crash away from
-- being wrong in the direction that matters.
--
-- trace_id and span_id are W3C Trace Context ids in lowercase hex, which is what
-- makes a row joinable to a Jaeger trace; migration 00007's CHECK enforces that
-- shape, so a malformed id is refused rather than stored as an unjoinable
-- string. Both are nullable because a scheduled or startup-time action may
-- legitimately have no inbound trace.
--
-- NOTHING SECRET IS EVER PASSED HERE. Not a password, not a token, not a TOTP
-- secret or code, not an Authorization header. state_before/state_after carry
-- the CHANGED FIELDS ONLY -- a diff, not an entity dump -- and for the auth
-- actions they are null, because a login changes no persisted state and the only
-- fields it touches are ones that must never be written down.
--
-- name: InsertAuditEntry :exec
INSERT INTO audit_log (occurred_at, actor_kind, actor_id, action,
                       entity_type, entity_id, outcome, reason,
                       state_before, state_after,
                       trace_id, span_id, request_id, client_ip)
VALUES (@occurred_at, @actor_kind, @actor_id, @action,
        @entity_type, @entity_id, @outcome, @reason,
        @state_before, @state_after,
        @trace_id, @span_id, @request_id, @client_ip);
