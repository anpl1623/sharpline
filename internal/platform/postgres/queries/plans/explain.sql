/*
 * ============================================================================
 * EXPLAIN harness for every sqlc query -- driven by `make query-plans`
 * ============================================================================
 *
 * THIS FILE IS NOT A SQLC INPUT. sqlc.yaml lists its query files explicitly, and
 * this one is deliberately absent from that list. It lives one directory down for
 * the same reason.
 *
 * WHAT IT PROVES, against a throwaway database that is created seconds earlier and
 * destroyed seconds later:
 *
 *   1. Every query PREPAREs against the real schema. That is a stronger statement
 *      than "sqlc generated it": sqlc parses SQL with its own embedded parser and
 *      never contacts a server, so a query it accepts can still be rejected by
 *      Postgres. A successful PREPARE is Postgres itself parsing, resolving every
 *      column and function, and typing every parameter.
 *
 *   2. No plan sequentially scans a relation that grows without bound. Each plan is
 *      preceded by a marker line
 *
 *          @@@PLAN <QueryName>
 *
 *      and `make query-plans` fails if the block that follows contains a `Seq Scan
 *      on <rel>` for any relation outside QUERY_PLANS_SEQSCAN_OK in the Makefile --
 *      currently sports, leagues and books.
 *
 *      THE RULE IS PER-RELATION, NOT PER-QUERY, and that correction came from this
 *      harness's own first run. Marking whole queries "must use an index" flagged
 *      ListLeaguesInSport, FindLeagueBySlug and GetEventWithLeague, all of which
 *      sequentially scan `leagues` -- 8 rows here and bounded by how many leagues a
 *      provider covers in production. A sequential scan of a single-page table is
 *      the OPTIMAL plan, and an assertion that calls it a failure is an assertion
 *      that will be switched off. What must never be sequentially scanned is
 *      `events`, `markets`, `selections`, `prices` (or any of its chunks), and the
 *      ledger and wager tables -- all of which grow with the season and the
 *      customer base. Those indexes exist for the FK RESTRICT checks regardless of
 *      what the planner does with them here.
 *
 * ---------------------------------------------------------------------------
 * ABOUT THE DATA THIS GENERATES
 * ---------------------------------------------------------------------------
 * Section 1 generates rows with generate_series. That is NOT seeded fixture data
 * and it is not mock data: it is computed inside a container that the make target
 * destroys on exit, it never reaches the compose stack or any committed file, and
 * no application code can see it. It exists because an EXPLAIN against empty
 * tables is worthless -- the planner correctly prefers a sequential scan over a
 * zero-row table, so an index that IS used in production would be reported as
 * unused here. Volume is what makes the plans real.
 *
 * The shape is chosen to match what migration 00003 sizes the hypertable for:
 * ~240,000 price rows spread over about six and a half 12-hour chunks, so chunk
 * exclusion has something to exclude.
 *
 * ---------------------------------------------------------------------------
 * KEEP IN SYNC
 * ---------------------------------------------------------------------------
 * The PREPARE bodies in section 2 are copied VERBATIM from the `const` strings in
 * the generated .sql.go files under internal/platform/postgres/gen -- that is, from
 * the SQL after sqlc has rewritten @named parameters to $n, which is the exact text
 * pgx sends. If you change a query, change its twin here.
 *
 * (That path is spelled out rather than globbed for a reason worth knowing: a
 * slash-star sequence inside a block comment OPENS A NESTED COMMENT in PostgreSQL,
 * so writing the glob here silently swallowed the rest of this file -- including the
 * ON_ERROR_STOP setting -- and psql reported the error and still exited 0. Which is
 * why that setting is now passed on the command line instead.)
 *
 * That link is guarded in one direction and not the other, which is worth stating
 * plainly. `make query-plans` extracts every `-- name:` from the generated code and
 * fails if a query has no PREPARE here, so a NEW query cannot escape the harness.
 * A MODIFIED query body can, and only review catches it.
 * ============================================================================
 */

-- ON_ERROR_STOP is set on the psql COMMAND LINE by `make query-plans`, not here.
-- A setting inside this file can be swallowed by a parse error earlier in the file,
-- and psql without it reports the error and exits 0 -- which turns a broken harness
-- into a green build. It is belt-and-braces anyway: the target also requires the
-- @@@DONE sentinel at the end of the output and a non-zero count of plan blocks.
\timing off
\pset pager off

-- A custom plan is what pgx gets for the first executions of a statement and what
-- a value-dependent predicate needs to be planned well. Forcing it makes these
-- plans deterministic instead of dependent on execution count. The one place the
-- generic plan matters is re-tested explicitly at the end.
SET plan_cache_mode = force_custom_plan;


-- ============================================================================
-- SECTION 1 -- generate volume
--
-- One transaction, because ledger_entries carries a DEFERRABLE INITIALLY DEFERRED
-- balance trigger: the issuance half and the customer half of every grant must
-- reach COMMIT together or the transaction is refused. Two autocommitted INSERT
-- statements would fail on the first, which is the constraint working correctly.
-- ============================================================================

BEGIN;

INSERT INTO sports (id, slug, name) VALUES
    ('sport-basketball', 'basketball', 'Basketball'),
    ('sport-football',   'football',   'American Football');

INSERT INTO leagues (id, sport_id, slug, name)
SELECT 'league-' || g,
       CASE WHEN g % 2 = 0 THEN 'sport-basketball' ELSE 'sport-football' END,
       'league-' || g,
       'League ' || g
  FROM generate_series(1, 8) AS g;

INSERT INTO books (id, slug, name, kind, is_reference)
SELECT 'book-' || g,
       'book-' || g,
       'Book ' || g,
       CASE WHEN g = 10 THEN 'synthetic' ELSE 'external' END,
       g = 1
  FROM generate_series(1, 10) AS g;

-- 2,000 events across 8 leagues. Status is spread across the lifecycle so the
-- PARTIAL indexes on events have rows both inside and outside their predicate --
-- an index whose predicate excludes nothing proves nothing.
INSERT INTO events (id, league_id, kind, name,
                    home_competitor_name, away_competitor_name,
                    scheduled_start, status, observed_at)
SELECT 'event-' || g,
       'league-' || (1 + (g % 8)),
       'match',
       'Event ' || g,
       'Home Team ' || g,
       'Away Team ' || g,
       now() + ((g % 720) || ' minutes')::interval,
       (ARRAY['scheduled', 'live', 'suspended', 'ended', 'settled'])[1 + (g % 5)],
       now()
  FROM generate_series(1, 2000) AS g;

-- Three markets per event: a moneyline (line must be NULL), a spread (line
-- required) and a total (line required and positive). markets_line_rule enforces
-- each of those, so this also exercises the CHECK set.
INSERT INTO markets (id, event_id, type, line, status, observed_at)
SELECT 'market-' || g || '-ml', 'event-' || g, 'moneyline', NULL,  'open', now()
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-sp', 'event-' || g, 'spread',    -3.5,  'open', now()
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-to', 'event-' || g, 'total',     214.5, 'open', now()
  FROM generate_series(1, 2000) AS g;

-- Two selections per market, with roles legal for the parent's type
-- (selections_role_allowed).
INSERT INTO selections (id, market_id, market_type, role, name)
SELECT 'market-' || g || '-ml-home', 'market-' || g || '-ml', 'moneyline', 'home',  'Home'
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-ml-away', 'market-' || g || '-ml', 'moneyline', 'away',  'Away'
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-sp-home', 'market-' || g || '-sp', 'spread',    'home',  'Home -3.5'
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-sp-away', 'market-' || g || '-sp', 'spread',    'away',  'Away +3.5'
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-to-over', 'market-' || g || '-to', 'total',     'over',  'Over 214.5'
  FROM generate_series(1, 2000) AS g
UNION ALL
SELECT 'market-' || g || '-to-under','market-' || g || '-to', 'total',     'under', 'Under 214.5'
  FROM generate_series(1, 2000) AS g;

-- 600 selections x 10 books x 40 observation times = 240,000 price rows, spread
-- back over 78 hours, i.e. six and a half 12-hour chunks.
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
SELECT s.id,
       'book-' || b,
       1.30 + (((s.ord * 7 + b * 13 + t) % 250)::double precision / 100.0),
       CASE WHEN s.id LIKE '%-sp-%' THEN -3.5
            WHEN s.id LIKE '%-to-%' THEN 214.5
            ELSE NULL END,
       now() - ((t * 2) || ' hours')::interval,
       now() - ((t * 2) || ' hours')::interval + interval '180 milliseconds'
  FROM (SELECT id, row_number() OVER (ORDER BY id) AS ord
          FROM selections
         ORDER BY id
         LIMIT 600) AS s
 CROSS JOIN generate_series(1, 10) AS b
 CROSS JOIN generate_series(0, 39) AS t;

INSERT INTO users (id, email, password_hash, password_changed_at)
SELECT 'user-' || g,
       'user' || g || '@example.test',
       '$argon2id$v=19$m=65536,t=3,p=4$' || repeat('a', 22) || '$' || repeat('b', 43),
       now() - interval '30 days'
  FROM generate_series(1, 500) AS g;

-- 5,000 balanced grant transactions -> 10,000 ledger entries, ~20 per customer
-- account, which is what gives ledger_entries_account_idx a selective range to
-- scan for GetAccountBalance.
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at)
SELECT 'txn-' || u || '-' || n, 'grant', NULL, now() - ((n || ' days')::interval)
  FROM generate_series(1, 500) AS u
 CROSS JOIN generate_series(1, 10) AS n;

INSERT INTO ledger_entries (transaction_id, entry_index, account_kind,
                            account_user_id, amount_minor, kind, occurred_at)
SELECT 'txn-' || u || '-' || n, 0, 'issuance', NULL,
       -(1000 * n), 'grant', now() - ((n || ' days')::interval)
  FROM generate_series(1, 500) AS u
 CROSS JOIN generate_series(1, 10) AS n
UNION ALL
SELECT 'txn-' || u || '-' || n, 1, 'user_cash', 'user-' || u,
       (1000 * n), 'grant', now() - ((n || ' days')::interval)
  FROM generate_series(1, 500) AS u
 CROSS JOIN generate_series(1, 10) AS n;

COMMIT;

-- Without statistics every plan below is a guess.
ANALYZE;

\echo ''
\echo '@@@ROWCOUNTS'
SELECT 'sports' AS relation, count(*) FROM sports
UNION ALL SELECT 'leagues',             count(*) FROM leagues
UNION ALL SELECT 'books',               count(*) FROM books
UNION ALL SELECT 'events',              count(*) FROM events
UNION ALL SELECT 'markets',             count(*) FROM markets
UNION ALL SELECT 'selections',          count(*) FROM selections
UNION ALL SELECT 'prices',              count(*) FROM prices
UNION ALL SELECT 'users',               count(*) FROM users
UNION ALL SELECT 'ledger_transactions', count(*) FROM ledger_transactions
UNION ALL SELECT 'ledger_entries',      count(*) FROM ledger_entries
ORDER BY 1;

\echo ''
\echo '@@@CHUNKS'
SELECT count(*) AS price_chunks
  FROM timescaledb_information.chunks
 WHERE hypertable_name = 'prices';


-- ============================================================================
-- SECTION 2 -- PREPARE every query, verbatim from the generated code
--
-- A failure here fails the whole target (ON_ERROR_STOP), and that is the point:
-- it is Postgres, not sqlc's parser, accepting every statement the application
-- will send.
-- ============================================================================

-- catalogue.sql -------------------------------------------------------------

PREPARE q_ListSports AS
SELECT id, slug, name
  FROM sports
 ORDER BY name;

PREPARE q_ListLeaguesInSport (text) AS
SELECT id, sport_id, slug, name
  FROM leagues
 WHERE sport_id = $1
 ORDER BY name;

PREPARE q_FindLeagueBySlug (text) AS
SELECT id, sport_id, slug, name
  FROM leagues
 WHERE slug = $1;

PREPARE q_ListBooks AS
SELECT id, slug, name, kind, is_reference
  FROM books
 ORDER BY slug;

PREPARE q_ListOpenEventsInLeague (text, bigint) AS
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE league_id = $1
   AND status IN ('scheduled', 'live', 'suspended')
 ORDER BY scheduled_start
 LIMIT $2;

PREPARE q_ListOpenEventsStartingBefore (timestamptz, bigint) AS
SELECT id, league_id, kind, name,
       home_competitor_id, home_competitor_name,
       away_competitor_id, away_competitor_name,
       scheduled_start, status,
       clock_period, clock_elapsed_ns, clock_running,
       score_home, score_away, observed_at
  FROM events
 WHERE status IN ('scheduled', 'live', 'suspended')
   AND scheduled_start < $1
 ORDER BY scheduled_start
 LIMIT $2;

PREPARE q_GetEventWithLeague (text) AS
SELECT e.id, e.league_id, e.kind, e.name,
       e.home_competitor_id, e.home_competitor_name,
       e.away_competitor_id, e.away_competitor_name,
       e.scheduled_start, e.status,
       e.clock_period, e.clock_elapsed_ns, e.clock_running,
       e.score_home, e.score_away, e.observed_at,
       l.slug AS league_slug,
       l.name AS league_name,
       s.id   AS sport_id,
       s.slug AS sport_slug,
       s.name AS sport_name
  FROM events  e
  JOIN leagues l ON l.id = e.league_id
  JOIN sports  s ON s.id = l.sport_id
 WHERE e.id = $1;

PREPARE q_ListMarketsForEvent (text) AS
SELECT id, event_id, type, line, subject, status, observed_at
  FROM markets
 WHERE event_id = $1
 ORDER BY type, id;

PREPARE q_ListSelectionsForMarkets (text[]) AS
SELECT id, market_id, market_type, role, name
  FROM selections
 WHERE market_id = ANY($1::TEXT[]);

PREPARE q_SearchOpenEventsByCompetitorPrefix (bigint, text) AS
SELECT id, league_id, kind, name,
       home_competitor_name, away_competitor_name,
       scheduled_start, status
  FROM events
 WHERE home_competitor_name IS NOT NULL
   AND lower(home_competitor_name) LIKE lower($2::TEXT) || '%'
UNION
SELECT id, league_id, kind, name,
       home_competitor_name, away_competitor_name,
       scheduled_start, status
  FROM events
 WHERE away_competitor_name IS NOT NULL
   AND lower(away_competitor_name) LIKE lower($2::TEXT) || '%'
 ORDER BY scheduled_start
 LIMIT $1;

-- prices.sql ----------------------------------------------------------------

PREPARE q_LatestPriceForEachBookOnSelections (text[], timestamptz) AS
SELECT DISTINCT ON (selection_id, book_id)
       selection_id,
       book_id,
       decimal_odds,
       line,
       observed_at,
       ingested_at
  FROM prices
 WHERE selection_id = ANY($1::TEXT[])
   AND observed_at > $2
 ORDER BY selection_id, book_id, observed_at DESC;

PREPARE q_ListPriceHistoryForSelectionAtBook (text, text, timestamptz, timestamptz) AS
SELECT selection_id,
       book_id,
       decimal_odds,
       line,
       observed_at,
       ingested_at
  FROM prices
 WHERE selection_id = $1
   AND book_id = $2
   AND observed_at >= $3
   AND observed_at <  $4
 ORDER BY observed_at;

PREPARE q_InsertPrice (text, text, double precision, double precision,
                       timestamptz, timestamptz) AS
INSERT INTO prices (selection_id, book_id, decimal_odds, line, observed_at, ingested_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (selection_id, book_id, observed_at) DO NOTHING;

-- betting.sql ---------------------------------------------------------------

PREPARE q_GetAccountBalance (text, text) AS
SELECT account_kind,
       account_user_id,
       balance_minor,
       entry_count
  FROM account_balances
 WHERE account_kind = $1
   AND account_user_id = $2;

PREPARE q_InsertRoundRobin (text, text, integer, bigint, timestamptz) AS
INSERT INTO round_robins (id, user_id, selection_count,
                          stake_per_combination_minor, placed_at)
VALUES ($1, $2, $3, $4, $5);

PREPARE q_InsertRoundRobinSize (text, integer, integer) AS
INSERT INTO round_robin_sizes (round_robin_id, selection_count, size)
VALUES ($1, $2, $3);

PREPARE q_InsertWager (text, text, text, text,
                       bigint, double precision, text,
                       bigint, bigint,
                       double precision, text,
                       bigint, bigint,
                       timestamptz, timestamptz) AS
INSERT INTO wagers (id, user_id, kind, status,
                    stake_minor, accepted_decimal, rounding,
                    potential_payout_minor, potential_profit_minor,
                    teaser_points, round_robin_id,
                    returned_minor, net_return_minor,
                    placed_at, transitioned_at)
VALUES ($1, $2, $3, $4,
        $5, $6, $7,
        $8, $9,
        $10, $11,
        $12, $13,
        $14, $15);

PREPARE q_InsertWagerLeg (text, text, text, text, text,
                          text, text,
                          text, double precision, double precision, timestamptz,
                          double precision, text, timestamptz) AS
INSERT INTO legs (id, wager_id, event_id, market_id, market_type,
                  selection_id, role,
                  price_book_id, price_decimal, price_line, price_observed_at,
                  teased_line, status, graded_at)
VALUES ($1, $2, $3, $4, $5,
        $6, $7,
        $8, $9, $10, $11,
        $12, $13, $14);

PREPARE q_InsertLedgerTransaction (text, text, text, timestamptz) AS
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at)
VALUES ($1, $2, $3, $4);

PREPARE q_InsertLedgerEntry (text, integer, text, text,
                             bigint, text, timestamptz) AS
INSERT INTO ledger_entries (transaction_id, entry_index,
                            account_kind, account_user_id,
                            amount_minor, kind, occurred_at)
VALUES ($1, $2,
        $3, $4,
        $5, $6, $7);

\echo ''
\echo '@@@PREPARED'
SELECT count(*) AS prepared_statements FROM pg_prepared_statements;


-- ============================================================================
-- SECTION 3 -- the plans
-- ============================================================================

\echo ''
\echo '@@@PLAN ListSports'
\echo '@@@NOTE No predicate. A full scan of `sports` is the optimal plan.'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListSports;

\echo ''
\echo '@@@PLAN ListBooks'
\echo '@@@NOTE No predicate. A full scan of `books` is the optimal plan.'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListBooks;

\echo ''
\echo '@@@PLAN ListLeaguesInSport'
\echo '@@@NOTE leagues_sport_idx exists for the FK check; the planner will scan 8 rows instead.'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListLeaguesInSport('sport-basketball');

\echo ''
\echo '@@@PLAN FindLeagueBySlug'
\echo '@@@NOTE the UNIQUE index on leagues.slug guarantees one row; the planner may still scan it.'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_FindLeagueBySlug('league-5');

\echo ''
\echo '@@@PLAN ListOpenEventsInLeague'
\echo '@@@NOTE expects events_league_board_idx, and NO Sort node'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListOpenEventsInLeague('league-5', 50);

\echo ''
\echo '@@@PLAN ListOpenEventsStartingBefore'
\echo '@@@NOTE expects events_open_start_idx, and NO Sort node'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListOpenEventsStartingBefore(now() + interval '3 hours', 50);

\echo ''
\echo '@@@PLAN GetEventWithLeague'
\echo '@@@NOTE expects events_pkey and sports_pkey; `leagues` is small enough to scan.'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_GetEventWithLeague('event-1234');

\echo ''
\echo '@@@PLAN ListMarketsForEvent'
\echo '@@@NOTE expects markets_event_type_idx'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListMarketsForEvent('event-1234');

\echo ''
\echo '@@@PLAN ListSelectionsForMarkets'
\echo '@@@NOTE expects selections_market_idx'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListSelectionsForMarkets(
    ARRAY['market-1234-ml', 'market-1234-sp', 'market-1234-to']::text[]);

\echo ''
\echo '@@@PLAN SearchOpenEventsByCompetitorPrefix'
\echo '@@@NOTE expects both events_*_competitor_search_idx partial expression indexes'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_SearchOpenEventsByCompetitorPrefix(50, 'Home Team 12');

\echo ''
\echo '@@@PLAN LatestPriceForEachBookOnSelections'
\echo '@@@NOTE THE HOTTEST QUERY. expects prices_natural_key_idx, chunk exclusion, and NO Sort'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_LatestPriceForEachBookOnSelections(
    ARRAY['market-1-ml-home', 'market-1-ml-away',
          'market-1-sp-home', 'market-1-sp-away',
          'market-1-to-over', 'market-1-to-under']::text[],
    now() - interval '1 hour');

\echo ''
\echo '@@@PLAN ListPriceHistoryForSelectionAtBook'
\echo '@@@NOTE expects one contiguous range scan of prices_natural_key_idx, NO Sort'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_ListPriceHistoryForSelectionAtBook(
    'market-1-ml-home', 'book-3',
    now() - interval '48 hours', now());

\echo ''
\echo '@@@PLAN GetAccountBalance'
\echo '@@@NOTE expects an INDEX-ONLY scan of ledger_entries_account_idx below the aggregate'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_GetAccountBalance('user_cash', 'user-250');

-- No ANALYZE: this one would actually write a row, and the interesting part of the
-- plan is which index Postgres picked as the ON CONFLICT arbiter.
\echo ''
\echo '@@@PLAN InsertPrice'
\echo '@@@NOTE expects Conflict Arbiter Indexes: prices_natural_key_idx (no ANALYZE: it would insert)'
EXPLAIN (VERBOSE) EXECUTE q_InsertPrice(
    'market-1-ml-home', 'book-3', 1.91, NULL, now(), now());

-- The six remaining writes have no plan worth asserting on -- an INSERT of one row
-- with no subquery is `Insert on <table>` over a `Result`. They are PREPAREd in
-- section 2, which is the check that matters for them: Postgres accepted the
-- statement, resolved every column, and typed every parameter.
\echo ''
\echo '@@@PLAN InsertWager'
\echo '@@@NOTE single-row insert, nothing to scan; the PREPARE in section 2 is the real check.'
EXPLAIN EXECUTE q_InsertWager('wager-x', 'user-1', 'straight', 'placed',
                              1000, 1.91, 'half_to_even',
                              1910, 910,
                              NULL, NULL,
                              NULL, NULL,
                              now(), now());


-- ============================================================================
-- SECTION 4 -- the generic-plan caveat, tested rather than asserted
--
-- pgx caches prepared statements, and after five executions Postgres may switch a
-- statement from a per-value CUSTOM plan to a value-blind GENERIC one. That is
-- harmless for an equality predicate, but a prefix LIKE is exactly the case where
-- it can matter: the planner derives the index range bounds FROM the pattern, and
-- a generic plan does not have the pattern.
--
-- This section forces the generic plan and prints what happens, so the answer is
-- measured instead of assumed. It is NOT asserted on -- read the output.
-- ============================================================================

SET plan_cache_mode = force_generic_plan;

\echo ''
\echo '@@@GENERIC SearchOpenEventsByCompetitorPrefix'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_SearchOpenEventsByCompetitorPrefix(50, 'Home Team 12');

\echo ''
\echo '@@@GENERIC LatestPriceForEachBookOnSelections'
EXPLAIN (ANALYZE, BUFFERS) EXECUTE q_LatestPriceForEachBookOnSelections(
    ARRAY['market-1-ml-home', 'market-1-ml-away',
          'market-1-sp-home', 'market-1-sp-away',
          'market-1-to-over', 'market-1-to-under']::text[],
    now() - interval '1 hour');

RESET plan_cache_mode;

\echo ''
\echo '@@@DONE'
