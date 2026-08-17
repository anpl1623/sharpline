-- =============================================================================
-- 00003  prices  (THE hypertable)
-- =============================================================================
--
-- CLAUDE.md §4, on Price: "odds for a selection at a book at an instant.
-- Immutable; a new price is a new row. THIS IS THE HYPERTABLE."
--
-- CLAUDE.md §3: "Postgres 17 + TimescaleDB -- relational core plus hypertables
-- for the odds time-series. Line history is the interesting dataset (CLV, steam
-- detection, book disagreement) and it is inherently time-series."
--
-- This file creates the table, converts it to a TimescaleDB hypertable
-- partitioned on the PROVIDER'S observation instant, enforces immutability in
-- the database rather than by convention, and creates exactly one index.
--
-- The compression settings, the compression policy, and the deliberate absence
-- of a retention policy and of a continuous aggregate all live in
-- 00004_timescale_policies.sql. The split is not cosmetic: `ALTER TABLE ... SET
-- (timescaledb.compress = false)` FAILS while any chunk is still compressed
-- (verified: "cannot disable columnstore on hypertable with columnstore
-- chunks"), so changing `compress_segmentby` later means rolling back one
-- migration that decompresses everything and re-applying it. Keeping that
-- operation in its own file means retuning compression never touches the table
-- definition, and this migration's Down never has to think about it.
--
-- Depends on:
--   00001  timescaledb extension, and the authoritative ENUM CATALOGUE
--   00002  selections(id) and books(id) -- the two axes a Price is keyed on
--
--
-- =============================================================================
-- WHAT A ROW IS, AND WHY EACH COLUMN EXISTS
-- =============================================================================
--
-- domain.Price {selectionID, bookID, decimal, line, observedAt}, plus two
-- schema-only timestamps that exist for the observability contract.
--
-- THE PRICE IS STORED AS DECIMAL ODDS AND NOTHING ELSE.
-- price.go: "Decimal is the only format that is total over the useful range:
-- American odds are undefined between -100 and +100 and have a sign
-- discontinuity at even money, and fractional odds need a rational rather than
-- a float." The phase-1 handoff is blunter still -- "American and Fractional
-- are DISPLAY formats: convert at the edge, render, discard." So there is no
-- american_odds column, no fractional numerator/denominator, and no format
-- discriminator. CLAUDE.md §6's odds-format toggle is a rendering concern that
-- reads this column through internal/domain/odds and never reaches back here.
--
-- THE LINE IS ON THE PRICE, NOT ONLY ON THE MARKET, AND THAT IS THE POINT.
-- markets.line (00002) carries the market's CURRENT line. This column carries
-- the line THE QUOTE WAS MADE AT. They are different facts and the second one
-- is the whole reason this table is interesting. price.go states the failure
-- mode exactly: "'-3.5 at 1.91' followed by '-4 at 1.95' is a line move, while
-- the same two prices with the line stripped look like a pure odds move, and
-- CLV computed against a closing price at a different line is not CLV."
--
-- Note the perspective difference from 00002, which matters when reading the
-- two columns together. markets.line is stated from the HOME side. This column
-- holds domain.Price.Line, which PriceParams documents as "the handicap or
-- threshold the quote was made at, FROM THE SELECTION'S OWN PERSPECTIVE -- the
-- value EffectiveLine returns, already inverted for an away spread". So the
-- away side of a -3.5 home spread stores +3.5 here. That is deliberate: a price
-- must be self-contained, because CLV and steam detection compare two prices
-- for the SAME selection and neither should have to re-derive an inversion from
-- a market row that has since moved.
--
--
-- TWO TIMESTAMPS, AND WHY NEITHER IS OPTIONAL
-- ---------------------------------------------------------------------------
-- Read deploy/observability/grafana/dashboards/sharpline-overview.json before
-- renaming either of these. Its headline-SLO text panel defines:
--
--     staleness = instant the price is written to the client socket
--               - provider observation timestamp carried on that price
--
--   "Measured by `stream`, at the moment of fanout [...] The provider
--    observation timestamp is the provider's own `last_update` for the market,
--    propagated UNCHANGED through ingest -> Kafka -> pricer -> stream. No hop
--    re-stamps it."
--
--   observed_at   The provider's own `last_update`. domain.Price.ObservedAt(),
--                 normalised to UTC by NewPrice. The PARTITIONING COLUMN, the
--                 subtrahend in every staleness measurement, and the event-time
--                 attribute phase 12's Flink watermarks will be assigned from.
--                 It is an OBSERVATION instant and must never be stamped with a
--                 clock reading of our own -- PriceParams says so in as many
--                 words: "it is the hypertable's time dimension, so it must be
--                 an observation instant and never an insertion instant."
--
--   ingested_at   When `ingest` received the payload carrying this quote. This
--                 is the instant behind the dashboard's stage="received"
--                 series: `sharpline_odds_staleness_seconds{stage="received"}`
--                 is (ingested_at - observed_at). Keeping it on the row means
--                 that number is recomputable from storage after the fact,
--                 which is what turns "the SLO was breached last Tuesday" from
--                 a shrug into an attribution.
--
--   created_at    When THIS ROW was committed, from the database's own clock.
--                 The repo-wide convention requires it, and here it earns its
--                 place independently: the writer is a Kafka consumer, so
--                 (created_at - ingested_at) is bus lag plus write latency
--                 made durable, and a replayed partition writes rows whose
--                 created_at is hours after ingested_at. 00007 uses exactly
--                 this pairing on audit_log for exactly this reason.
--
-- The two subtractions answer different questions and that is why both columns
-- are kept:
--
--   ingested_at - observed_at   PROVIDER-ATTRIBUTABLE latency. At ADR 0003's
--                               recommended tier this is dominated by the
--                               90-second polling cadence and is a floor
--                               nothing in this architecture can lower. ADR 0003
--                               is explicit that this is the honest number:
--                               "the staleness SLO is now provider-bound, not
--                               architecture-bound [...] The Grafana dashboard
--                               must distinguish provider staleness from
--                               pipeline staleness or it will measure the wrong
--                               thing and flatter the wrong component."
--   created_at  - ingested_at   OURS. Kafka lag plus the Timescale writer.
--
-- Collapsing them into one column would make exactly the mistake the ADR warns
-- about, permanently and in storage.
--
-- Cost, MEASURED rather than waved away, because it is not negligible. Three
-- TIMESTAMPTZ columns is 24 bytes per row uncompressed on the highest-volume
-- table in the schema. Under 00004's compression settings, on the same 202,800
-- row probe:
--
--     observed_at only                        18.34 bytes/row
--     + ingested_at                           21.69 bytes/row   (+3.35)
--     + created_at            <-- shipped     23.71 bytes/row   (+2.02)
--
-- So the two extra timestamps are 5.37 bytes/row, or 29% of the compressed row.
-- Delta-delta encoding gets them down from 16 bytes to 5.37 -- a 3x squeeze,
-- because they are near-monotonic within a segment -- but "very nearly free"
-- would be a false claim and is not made.
--
-- The trade is taken with the real number in hand: over a decade at the
-- operating tier that is 35 GB of history instead of 27 GB, on a 200 GB volume,
-- in exchange for being able to attribute any staleness breach to the provider
-- or to our own pipeline from STORAGE, permanently, after the Prometheus
-- retention window (15 days, per compose.obs.yaml) has long expired. 8 GB per
-- decade for a permanently answerable question about the project's headline SLO
-- is worth it.
--
-- There is deliberately NO `CHECK (ingested_at >= observed_at)`, and this is
-- not an oversight in a file that otherwise constrains everything it can.
--
--   * The two values come from two different clocks -- the PROVIDER's and the
--     ingest container's. A few hundred milliseconds of skew, or a provider
--     that stamps `last_update` slightly in the future, would make the
--     constraint reject a real price. 00007 declines the analogous check
--     between occurred_at and created_at on the same grounds.
--   * More importantly, the domain requires the skewed value to be STORABLE.
--     domain.Price.Age() returns a NEGATIVE duration for a future-stamped
--     observation, and price.go explains why: "returning it rather than
--     clamping to zero is what lets a monitor detect the skew instead of
--     silently reporting healthy staleness." A CHECK here would make clock skew
--     undetectable by making it unstorable, which is the opposite of the
--     intent.
--
-- The same argument declines `CHECK (created_at >= ingested_at)`.
--
--
-- WHY THERE IS NO updated_at
-- ---------------------------------------------------------------------------
-- The frozen convention is "every table gets created_at; MUTABLE tables also
-- get updated_at". This table is not mutable -- that is enforced below by
-- trigger, not asserted by comment -- so an updated_at column could only ever
-- hold a copy of created_at. There is also no set_updated_at trigger here, for
-- the same reason, which sidesteps the 00005/00007 disagreement that 00001
-- reports rather than resolves.
--
--
-- =============================================================================
-- CHUNK INTERVAL: 12 HOURS. THE ARITHMETIC.
-- =============================================================================
--
-- Not a round number picked for looking tidy. Two independent quantities decide
-- it: how many rows per day this system actually writes, and how much memory
-- PostgreSQL has been given.
--
-- ---------------------------------------------------------------------------
-- STEP 1 -- rows per day, from the real provider budget
-- ---------------------------------------------------------------------------
-- Row rate here is QUOTA-BOUND, not poll-rate-bound, which is what makes the
-- estimate tractable. Every input below is read out of
-- docs/adr/0003-odds-provider.md rather than invented:
--
--   Scenario C, "the recommended tier" ($59, 100K credits/month):
--       4 leagues, 3 featured markets (h2h/spreads/totals), 10 named
--       bookmakers (ADR requirement 1: `bookmakers=` counts groups of ten as
--       one region-equivalent, so ten books cost what one region costs),
--       live 5 h/day @ 90 s, pregame 8 h/day @ 15 min, distant 11 h/day @ 60 min.
--
--   Scenario D, "what a genuinely FanDuel-shaped board actually costs"
--   ($119, 5M credits/month):
--       6 leagues, 2 regions (~20 books), live 6 h/day @ 10 s,
--       pregame 12 h/day @ 120 s.
--
-- ADR fact 1 is what shapes the row count: "One `/odds` request returns every
-- upcoming and live event for that sport. Cost does NOT scale with the number
-- of events." So one sweep is a candidate write for EVERY (selection, book)
-- pair in that league's feed, and the feed is large even when the credit cost
-- is one request.
--
--   events visible per league feed, in season   NFL 16, NBA ~80, MLB ~105,
--                                               NHL ~60  ->  mean E = 65
--   selections on the three featured markets    h2h 2 + spreads 2 + totals 2 = 6
--
--   series per league  =  E x 6 x books
--       Scenario C:  65 x 6 x 10 =  3,900
--       Scenario D:  65 x 6 x 20 =  7,800
--
-- A sweep does not produce a row per series. CLAUDE.md §5 requires hashing each
-- normalized market to suppress no-op updates -- "most polls return identical
-- data and must not generate bus traffic" -- so a row exists only where the
-- quote actually CHANGED. Two regimes, because they differ by two orders of
-- magnitude:
--
--   in-play concurrency               ~6 events per league during the live window
--   in-play change rate per sweep     0.95 at a 90 s cadence (a liquid in-play
--                                     line has essentially always moved)
--                                     0.25 at a 10 s cadence (a given book
--                                     re-publishes far less often than that)
--   pregame change rate per sweep     0.15 @ 15 min, 0.20 @ 60 min,
--                                     0.005 @ 90 s, 0.02 @ 120 s
--
-- Scenario C, per league per day:
--     live sweeps    = 5 h / 90 s                        =    200
--     in-play rows   = (6 x 6 x 10) x 200 x 0.95         = 68,400
--     pregame rows   = 3,900 x (32x0.15 + 11x0.20 + 200x0.005)
--                    = 3,900 x 8.0                       = 31,200
--                                                          ───────
--                                                           99,600 rows/league/day
--     x 4 leagues                                        = 398,400
--
--                                              ==>  ~4.0 x 10^5 rows/day
--
-- Scenario D, per league per day:
--     live sweeps    = 6 h / 10 s                        =  2,160
--     in-play rows   = (6 x 6 x 20) x 2,160 x 0.25       = 388,800
--     pregame rows   = 7,800 x (360 x 0.02)              =  56,160
--                                                          ────────
--                                                          444,960 rows/league/day
--     x 6 leagues                                        = 2,669,760
--
--                                              ==>  ~2.7 x 10^6 rows/day
--
-- Player props (ADR scenario E) are a 5M-tier feature and add ~25,000 rows for
-- an NFL Sunday afternoon -- under 1% of the Scenario D day. They do not move
-- the sizing.
--
-- So the design envelope is a 7x range: ~4 x 10^5 rows/day at the tier this
-- project realistically runs on, ~2.7 x 10^6 rows/day at the tier that buys the
-- FanDuel-shaped board. The chunk interval must fit the CEILING, because a tier
-- upgrade must not require a migration.
--
-- ---------------------------------------------------------------------------
-- STEP 2 -- bytes per row, MEASURED
-- ---------------------------------------------------------------------------
-- Not estimated from the column list. 202,800 rows of realistically-shaped data
-- (1,560 distinct selections, 10 books, 15,600 series, mean selection_id length
-- 51.6 bytes, mean book_id length 9) were loaded into a to-scale prototype of
-- this exact table on the pinned image, and the hypertable measured:
--
--     hypertable_size = 51 MB / 202,800 rows  =  264.9 bytes/row
--
-- That figure includes the unique index below. Note what dominates it: two
-- provider-derived TEXT identifiers are ~62 of those bytes. That is the price of
-- the frozen "TEXT primary keys carrying the domain's own IDs" convention, paid
-- on the largest table in the schema. It is the right trade -- the ids have to
-- survive a rebuild and have to be able to key a compacted Kafka topic -- and
-- 00004's compression is what claws it back, because segmenting by selection_id
-- stores that identifier once per segment instead of once per row.
--
-- Column declaration order: MEASURED, and the readable order does cost
-- something. PostgreSQL lays attributes out in declaration order, so the two
-- leading varlena TEXT columns leave the following float8 needing alignment
-- padding. Measured with pg_column_size against a representative row (53-byte
-- selection_id, 10-byte book_id): the readable order below is 136 bytes where
-- declaring the five fixed-width 8-byte columns first is 129 -- 7 bytes, about
-- 2.6% of the uncompressed row.
--
-- The readable order is kept anyway, deliberately:
--   * The padding is not a constant. It is
--     (8 - ((len(selection_id)+1 + len(book_id)+1) mod 8)) mod 8, i.e. 0 to 7
--     bytes depending on the actual provider identifier lengths, so reordering
--     buys an amount nobody can predict.
--   * It is exactly ZERO once compressed. Compression stores columnar arrays
--     and has no per-row alignment padding, so the cost only exists in the
--     7-day uncompressed head -- roughly 19 MB of the operating tier's 740 MB.
--   * The declaration order mirrors domain.Price's field order, so the table
--     reads as the domain object it stores. Trading that for 19 MB, on a 200 GB
--     volume, would be the wrong optimisation.
--
-- ---------------------------------------------------------------------------
-- STEP 3 -- the memory budget
-- ---------------------------------------------------------------------------
-- TimescaleDB's guidance is to size a chunk so that the chunk being actively
-- written, plus its indexes, stays resident -- roughly 25% of main memory.
--
-- The real deploy target is NOT this laptop. Per the inter-agent ledger it is an
-- Oracle Cloud Always-Free VM.Standard.A1.Flex, 2 OCPU / 12 GB Ampere ARM, which
-- also runs Redis, Kafka and six Go services. Postgres's slice of that is ~2 GB,
-- and deploy/postgres/postgresql.conf is tuned to it:
--
--     shared_buffers        = 512MB      <-- the number that matters
--     effective_cache_size  = 1536MB
--
-- 512 MB is both `shared_buffers` and exactly 25% of Postgres's 2 GB container
-- budget, so the two ways of reading the guidance agree on the same target.
--
-- ---------------------------------------------------------------------------
-- STEP 4 -- the table
-- ---------------------------------------------------------------------------
--     interval | Scenario C chunk | Scenario D chunk        | chunks/year
--     ---------+------------------+-------------------------+------------
--       24 h   |     106 MB       |  715 MB   OVER BUDGET   |     365
--     > 12 h   |      53 MB       |  358 MB   70% of s_b    |     730
--        6 h   |      27 MB       |  179 MB   35% of s_b    |   1,460
--        1 h   |       4 MB       |   30 MB    6% of s_b    |   8,760
--
-- 24 hours fails outright at the ceiling: a 715 MB write-active chunk against
-- 512 MB of shared_buffers is a thrash, and the failure mode is a slow ingest
-- path, which shows up as the headline SLO degrading.
--
-- 6 hours passes with more headroom and is the honest runner-up. It is rejected
-- because it doubles the chunk count and doubles the number of chunks every
-- history query touches, for headroom that only matters in a scenario the
-- 12 GB box is not the target for.
--
-- 12 HOURS IS THE COARSEST INTERVAL THAT FITS THE CEILING INSIDE
-- shared_buffers, which is exactly the property to optimise for: chunks as
-- large as memory allows, so that queries touch as few as possible.
--
-- Query spans it produces:
--     live board, last 5 min                 1 chunk
--     line-movement chart, last 24 h         2-3 chunks
--     full pregame history, NFL game (7 d)   14 chunks
--     CLV (two point lookups)                2 chunks
--
-- Honest note on the boundary transient: around a chunk boundary two chunks are
-- briefly write-active, which at the Scenario D ceiling is 2 x 358 MB against
-- 512 MB. Accepted, because (a) the older chunk's pages are evicted quickly
-- since nothing writes to them again, and (b) Scenario D is a $119-tier
-- configuration, while at the tier this project runs the pair is 106 MB.
--
-- This is TUNABLE WITHOUT A REWRITE if measurement disagrees. `SELECT
-- set_chunk_time_interval('prices', INTERVAL '6 hours');` changes FUTURE chunks
-- only and leaves existing ones alone, so retuning is a one-line follow-up
-- migration rather than a data migration.
--
-- One caveat that is not in the arithmetic: the synthetic provider (ADR 0003's
-- no-key path) is not quota-bound at all, so in development the row rate is
-- whatever its tick rate is. That is fine -- `make down-hard` is the reset
-- button and no development volume is retained -- but a load test that runs the
-- synthetic generator flat out should not be read as a capacity measurement of
-- the real deployment.
--
--
-- =============================================================================
-- IMMUTABILITY IS A SCHEMA CONCERN, NOT A CONVENTION
-- =============================================================================
--
-- CLAUDE.md §4 says "Immutable; a new price is a new row." That is enforced
-- below by trigger. Leaving it as a convention would be a mistake specific to
-- this table for three reasons:
--
--   1. An UPDATE here silently destroys history, and history IS the product.
--      CLAUDE.md §3 calls line history "the interesting dataset"; §6 makes CLV
--      tracking and line-movement charts the differentiator. A single
--      `UPDATE prices SET decimal_odds = ...` to "fix" a bad quote rewrites the
--      past, and every CLV number computed before and after that statement
--      disagrees with no record of why.
--   2. It removes a whole class of writer bug by construction. The obvious way
--      to write an odds pipeline is upsert-on-current-price. That is the wrong
--      shape here, and the database now refuses it rather than quietly
--      accepting it and producing a table with one row per series.
--   3. The property has to hold for `make psql`, for a stray sqlc query, for a
--      service nobody has written yet, and for the operator at 2am -- not only
--      for today's disciplined code path.
--
-- TRIGGERS, NOT PRIVILEGES. `REVOKE UPDATE, DELETE` is the textbook answer and
-- it is insufficient for the same reason 00007 gives for audit_log: the
-- application connects as the database owner, and an owner's privileges on its
-- own table are self-restorable with one GRANT. A trigger fires for the owner
-- and for a superuser alike, and removing it requires a DDL statement that is
-- visible in a schema diff.
--
-- HOW THIS COEXISTS WITH TIMESCALE -- all four behaviours VERIFIED on the
-- pinned image (timescaledb 2.29.1, PostgreSQL 17.10), not assumed:
--
--   compress_chunk      SUCCEEDS with all three guards installed. This was the
--                       real risk in the design: if compression had gone
--                       through row DELETE or through TRUNCATE on the chunk,
--                       the guards would have silently made 00004's policy
--                       unable to ever run. It does not.
--   decompress_chunk    SUCCEEDS. It re-INSERTs, and INSERT is the one
--                       operation these guards permit.
--   drop_chunks         SUCCEEDS, because it is a DROP TABLE on a whole chunk
--                       and DROP TABLE fires no row trigger. This is the ONLY
--                       way rows can leave this table, and it can only remove a
--                       whole, explicitly named time range -- never a chosen
--                       row. 00004 declines to install a policy that uses it.
--   INSERT into a       SUCCEEDS, and the chunk stays compressed. The unique
--   COMPRESSED chunk    index is still enforced there (a duplicate is rejected
--                       with 23505 against the chunk's own index), and
--                       `ON CONFLICT DO NOTHING` works, which is what makes the
--                       writer's at-least-once Kafka redelivery idempotent even
--                       for a backfill landing in compressed history.
--
-- =============================================================================

-- +goose Up

-- -----------------------------------------------------------------------------
-- prices
-- -----------------------------------------------------------------------------
CREATE TABLE prices (
    -- domain.Price.SelectionID(). One of the two axes a price is keyed on.
    --
    -- ON DELETE RESTRICT, continuing 00002's choice down the last edge of the
    -- spine, and here the argument is at its strongest rather than merely
    -- consistent: this is the table 00002 was protecting when it refused
    -- CASCADE at every level. "Under CASCADE one DELETE against a stale league
    -- silently destroys every price ever observed under it." Nothing in the
    -- domain deletes a selection -- there is no Delete method anywhere in
    -- internal/domain -- so RESTRICT costs the system nothing it does today and
    -- makes retracting real market data a deliberate, ordered operation.
    --
    -- The charset CHECK reproduces domain.validID exactly, spelled identically
    -- to 00002's selections_id_charset and 00005's users_id_charset.
    selection_id  TEXT             NOT NULL
                                   REFERENCES selections (id) ON DELETE RESTRICT
                                   CONSTRAINT prices_selection_id_charset
                                   CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.Price.BookID(). The other axis. books is not part of the
    -- Sport->...->Selection chain (00002 says so); a price is the row where the
    -- two axes meet, which is why this is the only table with a foreign key to
    -- both.
    book_id       TEXT             NOT NULL
                                   REFERENCES books (id) ON DELETE RESTRICT
                                   CONSTRAINT prices_book_id_charset
                                   CHECK (book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.Price.Decimal(): total return per unit staked, stake included.
    -- DOUBLE PRECISION per CLAUDE.md §12 -- "odds and probabilities are floats;
    -- ledger amounts are not". This column is odds, so it is a float; nothing
    -- denominated in money appears in this table at all.
    --
    -- The bounds are domain.MinDecimalOdds/MaxDecimalOdds verbatim, with the
    -- same strictness: MinDecimalOdds is an EXCLUSIVE lower bound because 1.0
    -- "means the stake comes back and nothing else [...] and its implied
    -- probability is exactly 1.0, which divides by zero in the no-vig and Kelly
    -- formulas downstream." MaxDecimalOdds is INCLUSIVE and is a sanity guard,
    -- not a rule of the domain: it catches "an adapter reading an American
    -- price as a decimal one, or a cents field as an odds field."
    --
    -- THERE IS NO SEPARATE FINITENESS CHECK, AND THAT IS DELIBERATE. This
    -- range test already refuses NaN and both infinities, which is only true
    -- because of how PostgreSQL orders float8: NaN compares GREATER than every
    -- other value (unlike IEEE), so NaN passes `> 1.0` and then FAILS
    -- `<= 100000.0`; +Infinity fails the same way; -Infinity fails `> 1.0`.
    -- Verified for all three rather than reasoned about. Contrast `line` below,
    -- which is unbounded and therefore does need the explicit test.
    decimal_odds  DOUBLE PRECISION NOT NULL
                                   CONSTRAINT prices_decimal_odds_range
                                   CHECK (decimal_odds > 1.0
                                          AND decimal_odds <= 100000.0),

    -- domain.Price.Line(): the line THIS QUOTE was made at, from the
    -- SELECTION'S perspective (already inverted for an away spread -- see the
    -- header). NULL is domain.NoLine(); 0.0 is a stored pick'em.
    --
    -- Nullable rather than value-plus-present-flag, matching 00002's markets.line
    -- decision and for the same reason: the flag form admits a state that means
    -- nothing (present = false alongside a value of 4.5), and SQL already has a
    -- purpose-built encoding for optionality. The domain distinguishes the two
    -- cases and so does the schema.
    --
    -- The finiteness CHECK is 00002's markets_line_finite, character for
    -- character, including the reason the comparison is written as an ordering
    -- test rather than `line = line`: PostgreSQL defines NaN as EQUAL to itself,
    -- so self-equality would not catch it, while `line < 'Infinity'` does.
    line          DOUBLE PRECISION
                                   CONSTRAINT prices_line_finite
                                   CHECK (line IS NULL
                                          OR (line > '-Infinity'::double precision
                                              AND line < 'Infinity'::double precision)),

    -- THE PARTITIONING COLUMN. The provider's own observation instant,
    -- propagated unchanged from the provider payload; see the header for the
    -- Grafana SLO definition that depends on that word "unchanged".
    --
    -- The lower bound is spelled the same way as 00002's
    -- markets_observed_at_sane, and it does double duty here: it is also
    -- NewPrice's `ObservedAt.IsZero()` rejection expressed in SQL, since Go's
    -- zero time is year 1 and would fail this test.
    observed_at   TIMESTAMPTZ      NOT NULL
                                   CONSTRAINT prices_observed_at_sane
                                   CHECK (observed_at > '1900-01-01T00:00:00Z'),

    -- When `ingest` received the payload carrying this quote. The dashboard's
    -- stage="received" instant. Not a domain field: domain.Price models the
    -- provider's view of the world, and this is our view of when we learned it.
    ingested_at   TIMESTAMPTZ      NOT NULL
                                   CONSTRAINT prices_ingested_at_sane
                                   CHECK (ingested_at > '1900-01-01T00:00:00Z'),

    -- When this row was committed, from the database clock. No updated_at: this
    -- table is immutable, enforced below.
    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now()
);

COMMENT ON TABLE prices IS
    'CLAUDE.md §4: odds for a selection at a book at an instant. Immutable -- a new price is a new row, enforced by the prices_no_update/delete/truncate triggers. Hypertable partitioned on observed_at (the PROVIDER''s observation instant) with a 12-hour chunk interval; see the arithmetic in migrations/00003.';
COMMENT ON COLUMN prices.selection_id IS
    'domain.Price.SelectionID(). FK to selections(id) ON DELETE RESTRICT: this table is what 00002''s refusal to cascade was protecting.';
COMMENT ON COLUMN prices.book_id IS
    'domain.Price.BookID(). FK to books(id) ON DELETE RESTRICT. The second axis a price is keyed on; books is not part of the event hierarchy.';
COMMENT ON COLUMN prices.decimal_odds IS
    'domain.Price.Decimal(): total return per unit staked. Decimal is the canonical format -- American and fractional are DISPLAY formats converted at the edge, so no other odds representation is stored. Bounds are domain.MinDecimalOdds (exclusive) and MaxDecimalOdds (inclusive), which also refuse NaN and both infinities.';
COMMENT ON COLUMN prices.line IS
    'domain.Price.Line(): the line THIS QUOTE was made at, from the SELECTION''s perspective (already inverted for an away spread, unlike markets.line which is stated from the home side). NULL means no line; 0.0 is a real pick''em. Without this column a line move is indistinguishable from an odds move and CLV is wrong.';
COMMENT ON COLUMN prices.observed_at IS
    'The provider''s own last_update, propagated UNCHANGED through ingest -> Kafka -> pricer -> stream. Partitioning column, the subtrahend in the headline staleness SLO, and phase 12''s Flink event-time attribute. Never stamp this with a clock of ours.';
COMMENT ON COLUMN prices.ingested_at IS
    'When ingest received the payload carrying this quote. (ingested_at - observed_at) is the dashboard''s stage="received" staleness, i.e. the provider-attributable latency that ADR 0003 says is a floor this architecture cannot lower.';
COMMENT ON COLUMN prices.created_at IS
    'When this row was committed, from the database clock. (created_at - ingested_at) is bus lag plus write latency, made durable. Differs from ingested_at by hours on a Kafka replay.';

-- -----------------------------------------------------------------------------
-- Hypertable conversion.
--
-- Partitioned on observed_at with a 12-hour chunk interval -- see the arithmetic
-- in the header. `by_range` is the 2.x form, matching 00007's audit_log call so
-- the two hypertables in this schema are declared the same way.
--
-- create_default_indexes => FALSE, for the reason 00007 gives and one more of
-- its own. The default is an index on (observed_at DESC); on audit_log that
-- would have duplicated the primary key's leading column. Here it would not be
-- a duplicate -- it would be a genuinely second index -- and it is still
-- declined:
--
--   * It is a permanent write tax on 4 x 10^5 to 2.7 x 10^6 inserts per day for
--     a read pattern nothing in CLAUDE.md §6 asks for. Every product query
--     against this table filters on a selection first; none of them is
--     "everything, ordered by time".
--   * Time-range narrowing is what the PARTITIONING is for. A query bounded by
--     observed_at is already reduced to the relevant 12-hour chunks by chunk
--     exclusion before any index is consulted.
--   * Inside a compressed chunk it would not exist anyway. 00004 puts
--     observed_at in compress_orderby, which gives every compressed batch
--     min/max metadata on it -- that is the post-compression equivalent, and it
--     is free.
--   * The uncompressed working set is small: a 12-hour chunk is 53 MB at the
--     operating tier, so a scan inside one is cheap when it happens.
--
-- If a time-only access pattern later proves hot, `CREATE INDEX ON prices
-- (observed_at DESC)` is a one-line follow-up migration. Adding it speculatively
-- now is the write amplification 00002 warns about, on the one table where it
-- costs the most.
-- -----------------------------------------------------------------------------
SELECT create_hypertable(
    'prices',
    by_range('observed_at', INTERVAL '12 hours'),
    create_default_indexes => FALSE
);

-- -----------------------------------------------------------------------------
-- THE NATURAL KEY, AND WHY IT IS A UNIQUE INDEX RATHER THAN A PRIMARY KEY
-- -----------------------------------------------------------------------------
-- price.go states the key outright: "There is no PriceID. A price is identified
-- by (SelectionID, BookID, ObservedAt), which is exactly the TimescaleDB
-- hypertable's natural key. A surrogate key would add a uniqueness constraint
-- to maintain and a column to index without answering any question the natural
-- key does not."
--
-- A hypertable's PRIMARY KEY and every UNIQUE constraint on it MUST include the
-- partitioning column. That is satisfied here: observed_at is the third column.
--
-- But it is declared as `CREATE UNIQUE INDEX`, not `PRIMARY KEY`, and the reason
-- is the DESC:
--
--   * PostgreSQL does not permit ASC/DESC in PRIMARY KEY or UNIQUE constraint
--     syntax. `PRIMARY KEY (selection_id, book_id, observed_at DESC)` is a
--     syntax error. A descending column is only reachable through
--     CREATE INDEX.
--   * The DESC is not decoration. The hottest read in the system is "latest
--     price per selection per book", which is
--     `DISTINCT ON (selection_id, book_id) ... ORDER BY selection_id, book_id,
--      observed_at DESC`. Those pathkeys are neither the forward nor the reverse
--     of an all-ASC index, so an ASC-only index cannot produce them and the
--     planner has to sort. CLV's "last price at or before instant T" wants the
--     same descending scan.
--   * So the choice was: a PRIMARY KEY plus a second DESC index -- two indexes
--     to maintain on every one of up to 2.7 million daily inserts -- or one
--     unique index that is simultaneously the uniqueness constraint and the
--     ordering the reads need. One index wins, and it is not close.
--   * Nothing is given up by having no PK constraint object. Nothing in the
--     schema references prices: a Leg holds a COPIED domain.Price VALUE, never
--     an identifier that could re-resolve to a moved line (phase 1 handoff), so
--     no foreign key needs a narrower key to point at. sqlc does not require a
--     primary key. Logical replication would want a REPLICA IDENTITY, and a
--     unique index on NOT NULL columns can serve as one if that day comes.
--
-- WHAT THIS ONE INDEX SERVES -- all three required read patterns, plus CLV:
--
--   latest price per selection per book  (the odds board; the hottest query)
--       DISTINCT ON (selection_id, book_id) *
--       FROM prices
--       WHERE selection_id = ANY($1) AND observed_at > now() - INTERVAL '1 hour'
--       ORDER BY selection_id, book_id, observed_at DESC
--
--     THE TIME BOUND IS NOT OPTIONAL and callers must not omit it. A DISTINCT ON
--     with no lower bound on observed_at defeats chunk exclusion and forces the
--     planner to consult the index on EVERY chunk -- which is unbounded work
--     that grows for the life of the deployment, because 00004 deliberately
--     installs no retention policy. This is the single sharpest edge on this
--     table and it is why the note is here rather than in a code comment
--     somewhere.
--
--     Note also that this is Postgres serving as the SOURCE OF TRUTH, not as the
--     board's hot path. CLAUDE.md §3 puts the current-line snapshot in Redis and
--     in the compacted `odds.normalized` topic; this query is what a cold start
--     or a cache miss falls back to, and Redis is "never the source of truth".
--
--   full history for one selection  (the line-movement chart)
--       WHERE selection_id = $1 AND book_id = $2
--         AND observed_at >= $3 AND observed_at < $4
--       ORDER BY observed_at
--     Two leading equalities then a range on the third column: a single index
--     range scan, no sort.
--
--   cross-book comparison at an instant  (arbitrage and middle detection)
--       DISTINCT ON (selection_id, book_id) *
--       WHERE selection_id = ANY($1) AND observed_at <= $2
--       ORDER BY selection_id, book_id, observed_at DESC
--     The selection set comes from `selections WHERE market_id = $m`, which
--     00002's selections_market_idx serves. Measured at 0.073 ms against a
--     compressed 202,800-row chunk for a six-selection market across ten books.
--
--   CLV  (phase 9 in Go, phase 12 in Flink SQL)
--       WHERE selection_id = $1 AND book_id = $2 AND observed_at <= $closing
--       ORDER BY observed_at DESC LIMIT 1
--     Two equalities plus a backward walk from the boundary: one index probe.
--
-- DELIBERATELY ABSENT: an index led by book_id.
-- It would serve two things -- the referencing side of the book_id FK, so that
-- refusing `DELETE FROM books` is a lookup rather than a scan, and an
-- ops-flavoured "everything book X quoted in this window" query. Both are
-- rejected because the cost is permanent and the benefit is not:
--   * Nothing in the system deletes a book. When a book DOES have prices, the
--     RESTRICT check aborts on the first row it meets, so even a sequential scan
--     returns immediately; only proving the ABSENCE of references requires real
--     work, and that is a once-per-never admin operation.
--   * 00004 puts book_id first in compress_orderby, so compressed batches carry
--     min/max metadata on it and post-compression filtering by book prunes well.
--     Measured: a one-minute single-book slice of the compressed probe chunk
--     runs in 3.4 ms without this index. That is not a problem worth a second
--     index on the busiest write path in the schema.
-- -----------------------------------------------------------------------------
CREATE UNIQUE INDEX prices_natural_key_idx
    ON prices (selection_id, book_id, observed_at DESC);

COMMENT ON INDEX prices_natural_key_idx IS
    'domain.Price''s natural key (SelectionID, BookID, ObservedAt) -- the hypertable''s uniqueness constraint and, because of the trailing DESC, also the index that serves latest-price, line-movement, cross-book and CLV reads without a sort. Queries MUST bound observed_at or chunk exclusion is defeated and every chunk is consulted.';

-- -----------------------------------------------------------------------------
-- WHAT IS NOT CONSTRAINED HERE, AND WHY
-- -----------------------------------------------------------------------------
-- 00002 enforces domain.MarketType.LineRule() on `markets` -- a moneyline may
-- not carry a line, a total must carry a positive one -- and enforces
-- MarketType.AllowsRole() on `selections` by denormalising the parent's type and
-- pinning it with a composite foreign key to `markets (id, type)`.
--
-- The analogous constraint here would be "a price on a moneyline selection must
-- have line IS NULL", and it is NOT enforced, for a mechanical reason rather
-- than a philosophical one: a CHECK cannot read another table, so it would
-- require a denormalised market_type column on this table pinned by a composite
-- FK to `selections (id, market_type)` -- and `selections` has no UNIQUE
-- constraint on that pair, only a primary key on `id`. Adding one is a change to
-- 00002, which this migration does not own and must not edit.
--
-- It is also not obviously worth asking for. The cost is a fourth
-- provider-derived string on every row of the largest table in a schema with no
-- retention policy, plus a composite FK check on the hot write path. What it
-- buys is narrower than it first appears: the rule can only ever assert
-- "moneyline and futures ==> line IS NULL", because for a spread or a total the
-- price's line legitimately DIFFERS from the market's current line -- that
-- divergence is the entire reason this column exists.
--
-- The invariant is meanwhile enforced one level up and is hard to violate by
-- accident: markets_line_rule guarantees a moneyline market's line is NULL, and
-- the normalizer derives a price's line from its market's through
-- domain.EffectiveLine. Producing a moneyline price with a line requires a
-- normalizer that invented a line the market row does not have.
--
-- Recorded as an available option rather than a closed door: if a future phase
-- wants it, the change is `ALTER TABLE selections ADD CONSTRAINT
-- selections_id_market_type_key UNIQUE (id, market_type)` in a new migration,
-- then the copy and the FK here.

-- -----------------------------------------------------------------------------
-- APPEND-ONLY ENFORCEMENT
--
-- Namespaced `prices_` for the reason 00007 states and 00002 follows: a bare
-- name is the name every concurrently-authored migration reaches for, and the
-- first Down to run would drop a function another migration's triggers still
-- depend on. The prefix makes the blast radius of this file's Down exactly this
-- file.
--
-- SQLSTATE and HINT deliberately match 00007's audit_log_append_only so that a
-- caller handling one handles the other: 23001 restrict_violation.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION prices_append_only() RETURNS trigger
    LANGUAGE plpgsql
AS $prices_append_only$
BEGIN
    RAISE EXCEPTION 'prices is immutable: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation',
              HINT    = 'CLAUDE.md section 4: a new price is a new row. Correct a bad quote by '
                        'INSERTing the corrected observation, never by editing or removing the '
                        'original -- line history is the dataset, and rewriting it invalidates '
                        'every CLV number already computed from it. Bulk removal is a retention '
                        'decision: drop_chunks() on a named time range.';
    RETURN NULL;
END;
$prices_append_only$;
-- +goose StatementEnd

COMMENT ON FUNCTION prices_append_only() IS
    'Guard trigger: raises on any UPDATE, DELETE or TRUNCATE against prices. SQLSTATE 23001 (restrict_violation). Verified not to interfere with compress_chunk, decompress_chunk or drop_chunks.';

-- Row-level triggers on a hypertable are propagated to every chunk, so these
-- hold on the chunks that actually store the rows and not merely on the empty
-- root table. Verified against a three-chunk hypertable.
CREATE TRIGGER prices_no_update
    BEFORE UPDATE ON prices
    FOR EACH ROW EXECUTE FUNCTION prices_append_only();

CREATE TRIGGER prices_no_delete
    BEFORE DELETE ON prices
    FOR EACH ROW EXECUTE FUNCTION prices_append_only();

-- A row-level trigger does not fire for TRUNCATE, so the third guard is
-- statement-level and separate. Without it the two above are theatre.
CREATE TRIGGER prices_no_truncate
    BEFORE TRUNCATE ON prices
    FOR EACH STATEMENT EXECUTE FUNCTION prices_append_only();

-- -----------------------------------------------------------------------------
-- Postcondition. 00001 and 00007 both assert what they built; this asserts the
-- three facts every later migration and every sqlc query is entitled to assume,
-- so a wrong state is attributed here rather than surfacing as a mysterious
-- sequential scan or a missing chunk in phase 3.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
DO $postcondition$
DECLARE
    v_interval interval;
    v_dim      text;
BEGIN
    -- timescaledb_information.dimensions.time_interval is an INTERVAL for a
    -- time-based dimension (integer_interval is the column used for integer
    -- partitioning, which this table does not use).
    SELECT d.column_name, d.time_interval
      INTO v_dim, v_interval
      FROM timescaledb_information.dimensions d
     WHERE d.hypertable_name = 'prices';

    IF v_dim IS NULL THEN
        RAISE EXCEPTION 'prices was not registered as a hypertable'
            USING HINT = 'create_hypertable() returned without error but timescaledb_information.dimensions has no row.';
    END IF;

    IF v_dim <> 'observed_at' THEN
        RAISE EXCEPTION 'prices is partitioned on %, expected observed_at', v_dim
            USING HINT = 'The partitioning column must be the PROVIDER observation instant: it is the '
                         'staleness SLO''s subtrahend and phase 12''s Flink event-time attribute.';
    END IF;

    -- 12 hours. Compared as an interval VALUE rather than against a formatted
    -- string, so a display-format change in a future release cannot make this
    -- assertion pass vacuously.
    IF v_interval IS DISTINCT FROM INTERVAL '12 hours' THEN
        RAISE EXCEPTION 'prices chunk interval is %, expected 12 hours', v_interval::text
            USING HINT = 'See the sizing arithmetic in migrations/00003_prices_hypertable.sql. '
                         'Retune with set_chunk_time_interval() in a follow-up migration, not by editing this one.';
    END IF;

    IF (SELECT count(*) FROM pg_trigger
         WHERE tgrelid = 'prices'::regclass AND NOT tgisinternal) <> 3 THEN
        RAISE EXCEPTION 'prices should carry exactly 3 append-only guard triggers, found %',
            (SELECT count(*) FROM pg_trigger WHERE tgrelid = 'prices'::regclass AND NOT tgisinternal);
    END IF;

    RAISE NOTICE 'prices: hypertable on observed_at, 12h chunks, append-only guards installed';
END
$postcondition$;
-- +goose StatementEnd

-- +goose Down

-- -----------------------------------------------------------------------------
-- Reverses the Up exactly, and reverses nothing else.
--
-- DROP TABLE on a hypertable removes the hypertable, every chunk, every index on
-- it, and every trigger -- as 00007 notes, "no drop_hypertable() call is needed
-- or exists". It also works while chunks are compressed, verified, so this Down
-- is correct even if it is somehow reached with 00004 still applied (goose
-- unwinds in reverse order, so ordinarily 00004's Down has already decompressed
-- and unset compression before this runs).
--
-- The index is dropped explicitly ahead of the table, matching 00002's Down
-- style. It is redundant -- DROP TABLE would take it -- but it keeps the
-- inverse readable as a line-by-line mirror of the Up, and IF EXISTS makes the
-- redundancy harmless.
--
-- The trigger function is dropped LAST and unconditionally, because it is
-- namespaced to this migration and nothing outside this file may reference it.
-- -----------------------------------------------------------------------------
DROP INDEX IF EXISTS prices_natural_key_idx;

DROP TABLE IF EXISTS prices;

DROP FUNCTION IF EXISTS prices_append_only();
