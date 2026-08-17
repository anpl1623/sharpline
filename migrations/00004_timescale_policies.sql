-- =============================================================================
-- 00004  Timescale policies for `prices`
-- =============================================================================
--
-- This is what makes 00003 a TIME-SERIES STORE rather than a large table.
--
-- It does one thing and declines two, and the declining is as deliberate as the
-- doing. Both refusals are stated at length below so that nobody reads the
-- absence as an oversight and "fixes" it:
--
--   DOES     enables columnar compression, with segment_by and order_by chosen
--            by measurement rather than by habit, and installs a compression
--            policy at a justified age threshold.
--
--   DECLINES a RETENTION policy. CLAUDE.md §3 calls line history "the
--            interesting dataset"; §6 makes CLV tracking and line-movement
--            charts the differentiator. A retention policy deletes exactly that.
--            See "RETENTION" below -- the disk arithmetic says it is
--            unnecessary, not merely undesirable.
--
--   DECLINES a CONTINUOUS AGGREGATE. CLAUDE.md §3 lists "line-movement
--            aggregates -- tumbling windows sunk into Timescale" as a PHASE 12
--            Flink SQL job. Building one here duplicates a phase-12 deliverable
--            with a third implementation of a number phase 9 and phase 12 are
--            supposed to agree on. See "CONTINUOUS AGGREGATES" below.
--
-- WHY THIS IS A SEPARATE MIGRATION FROM 00003
-- ---------------------------------------------------------------------------
-- Not tidiness. Compression settings are the one part of this table that cannot
-- be altered in place: `ALTER TABLE prices SET (timescaledb.compress = false)`
-- FAILS while any chunk is still compressed. Verified on the pinned image:
--
--     ERROR:  cannot disable columnstore on hypertable with columnstore chunks
--
-- So retuning `compress_segmentby` means decompressing every chunk first. Having
-- that live in its own migration makes the retune `goose down 1` / edit /
-- `goose up` -- a bounded operation that never touches the table definition, the
-- indexes, or the append-only guards. Folding it into 00003 would make changing
-- one compression parameter a rollback of the entire prices table.
--
-- Depends on: 00003 (the prices hypertable).
--
--
-- =============================================================================
-- COMPRESSION: segment_by = selection_id, order_by = book_id, observed_at DESC
-- =============================================================================
--
-- segment_by is the load-bearing choice. It decides two things at once, and they
-- pull in opposite directions:
--
--   * COMPRESSION RATIO. Timescale compresses in batches of up to 1000 source
--     rows, one batch per segment value. Segment on too many columns and each
--     batch holds a handful of rows, where per-batch overhead dominates and the
--     ratio collapses.
--   * POST-COMPRESSION QUERY SPEED. segment_by columns are stored as plain
--     scalars on the compressed chunk and are indexed there, so an equality
--     filter on one is a direct lookup that decompresses nothing else. A column
--     that is NOT in segment_by can only be filtered after decompression, or
--     pruned approximately via order_by min/max metadata. Getting this wrong is
--     what turns a history query into a sequential scan.
--
-- The shape of this particular workload is what makes the choice non-obvious:
-- MANY series, each SHALLOW. At the operating tier a 12-hour chunk holds
-- ~200,000 rows spread over ~15,600 (selection, book) series -- about 13 rows
-- per series. Segmenting by the full series key therefore produces 13-row
-- batches, which is the pathological case.
--
-- MEASURED, not reasoned about. A to-scale prototype of 00003's exact table --
-- 202,800 rows, 1,560 distinct selections, 10 books, 15,600 series, mean
-- selection_id length 51.6 bytes, all inside one 12-hour chunk, i.e. a faithful
-- model of an operating-tier chunk -- was built on the pinned image
-- (timescaledb 2.29.1 / PostgreSQL 17.10) six times over, identical rows each
-- time, and compressed. Uncompressed baseline: 51 MB, 264.9 bytes/row.
--
--   segment_by            | order_by               | ratio  | B/row
--   ----------------------+------------------------+--------+------
--   selection_id, book_id | observed_at DESC       |  4.82x |  54.9
--   selection_id          | book_id, observed_at ↓ | 11.17x |  23.7   <== CHOSEN
--   selection_id          | observed_at ↓, book_id |  8.91x |  29.7
--   book_id               | selection_id, obs ↓    | 13.19x |  20.1
--   book_id, selection_id | observed_at DESC       |  4.79x |  55.3
--   (none)                | selection, book, obs ↓ | 15.28x |  17.3
--
-- Then the queries, on the SAME compressed chunk, 200 iterations each, using
-- the four real access patterns 00003 documents:
--
--   segment_by            | chart   | CLV     | cross-book | book slice
--   ----------------------+---------+---------+------------+-----------
--   selection_id (book,ts)| 0.020ms | 0.019ms |  0.073ms   |  3.43ms   <== CHOSEN
--   selection_id (ts,book)| 0.022ms | 0.017ms |  0.439ms   |  3.57ms
--   selection_id, book_id | 0.016ms | 0.009ms |  0.404ms   |  0.87ms
--   book_id               | 0.054ms | 0.039ms |  0.734ms   |  0.67ms
--
-- Reading those two tables together settles it:
--
--   * `segment_by = (selection_id, book_id)` -- the intuitive choice, since it
--     is the series key -- is the WORST option on storage at 4.82x, for the
--     13-rows-per-batch reason above. It also loses the cross-book query by
--     5.5x, because with two segment columns the compressed chunk has 15,600
--     segments to walk for a multi-selection predicate instead of 1,560.
--     Intuition is wrong here and the measurement is why this is written down.
--
--   * `segment_by = book_id` wins on ratio (13.19x) and it is REJECTED anyway.
--     Book cardinality is ten, so segments are large and compress beautifully --
--     but every product query against this table leads with a SELECTION, not a
--     book, and with book_id as the only segment column a selection predicate
--     has to walk all ten books' batches and prune on metadata. It loses
--     cross-book detection by 10x (0.734 vs 0.073 ms) and the chart and CLV
--     probes by ~2.5x, to save 15% of storage. Storage is the cheap axis here
--     (see the retention arithmetic below); latency on the arbitrage scanner is
--     not.
--
--   * `segment_by = (none)` is best on ratio and is not a real candidate: with
--     no segment column there is no post-compression equality filter at all.
--     Included in the table as the upper bound the chosen option is measured
--     against -- 11.17x versus a theoretical 15.28x is the price of keeping
--     selection_id directly filterable, and it is worth it.
--
--   * `order_by = 'book_id, observed_at DESC'` beats
--     `'observed_at DESC, book_id'` on both axes: better ratio (11.17x vs 8.91x,
--     because sorting by book first makes book_id run-length-encodable and
--     leaves observed_at monotone WITHIN each book for delta-delta encoding) and
--     6x better on cross-book (0.073 vs 0.439 ms). Putting the time column first
--     is the habit; here it is measurably wrong.
--
-- Why segmenting on selection_id compresses as well as it does: it removes the
-- single largest per-row cost this table has. 00003's measured 264.9 bytes/row
-- is dominated by two provider-derived TEXT identifiers, and segment_by stores
-- selection_id ONCE PER SEGMENT rather than once per row. The frozen TEXT-primary-
-- key convention is paid for on the write path and refunded here.
--
-- ONE COST ACCEPTED AND NAMED: the "book slice" query -- everything one book
-- quoted in a window, with no selection predicate -- goes from 0.67 ms to
-- 3.43 ms. That is an operations and QA query ("what did DraftKings actually
-- send us"), not a product query; nothing in CLAUDE.md §6 asks for it, and
-- 3.4 ms is not a problem. It is recorded so that if it ever becomes a product
-- query the tradeoff is visible rather than rediscovered.
--
-- INTERACTION WITH 00003's UNIQUE INDEX -- verified, because this is where
-- compression usually bites. TimescaleDB requires that the columns of a unique
-- index be covered by segment_by ∪ order_by. `prices_natural_key_idx` is
-- (selection_id, book_id, observed_at); segment_by contributes selection_id and
-- order_by contributes the other two, so the cover is exact and the ALTER is
-- accepted. Uniqueness remains ENFORCED on compressed chunks (a duplicate INSERT
-- into a compressed chunk is rejected against that chunk's own index), and
-- `ON CONFLICT DO NOTHING` works there too -- which is what makes the writer's
-- at-least-once Kafka redelivery idempotent even when a backfill lands in
-- compressed history.
--
--
-- =============================================================================
-- COMPRESSION AGE: 7 DAYS
-- =============================================================================
--
-- One honest sentence first, because the usual justification does not apply
-- here: THIS THRESHOLD IS NOT ABOUT READ PERFORMANCE. The benchmarks above were
-- run against a COMPRESSED chunk and the chart, CLV and cross-book probes came
-- in at 0.020, 0.019 and 0.073 ms. Compressed reads on this table are fast.
-- Claiming a week of uncompressed data is needed "so recent queries stay quick"
-- would be a story the measurement contradicts.
--
-- What the threshold is actually for:
--
--   1. THE OUT-OF-ORDER WRITE WINDOW -- the real reason. `observed_at` is the
--      provider's clock, not ours, so a row's partition key can be older than
--      wall clock by the polling cadence (90 s at ADR 0003's recommended tier)
--      plus pipeline lag. Worse, the writer is a Kafka consumer: a consumer
--      outage, a rebalance, or a deliberate offset reset replays hours or days
--      of `odds.normalized` at once, and every replayed row lands in the chunk
--      its observation instant belongs to. Inserting into a compressed chunk
--      works (verified) but it is materially slower than inserting into a plain
--      heap, and a replay is exactly when the pipeline is already behind and the
--      staleness SLO is already at risk. 7 days is 14 chunk intervals, which
--      keeps the entire realistic replay window on plain heap.
--
--   2. INCIDENT FORENSICS. A week is the window in which someone actually reads
--      raw rows by hand through `make psql` after a staleness breach or a
--      suspicious line. Uncompressed rows are inspectable with ordinary tools
--      and ordinary EXPLAIN output.
--
--   3. IT KEEPS THE BACKGROUND JOB CHEAP. At a 12-hour chunk interval only two
--      chunks per day ever become newly eligible, so most policy runs find
--      nothing to do. On a 2 OCPU box shared with Kafka, Redis and six Go
--      services, a compression job that is usually a no-op is the correct
--      shape.
--
-- Cost: 7 days of uncompressed data on disk -- ~740 MB at the operating tier
-- (106 MB/day) and ~5.0 GB at the Scenario D ceiling (715 MB/day). Both are
-- comfortable on the deploy target's 200 GB block volume, and only the newest
-- chunk is write-hot, so `shared_buffers` is not under pressure from the other
-- thirteen.
--
-- CROSS-PHASE CONSTRAINT THIS CREATES: phase 3 owns Kafka topic retention via
-- Terraform (CLAUDE.md §9 -- "Topics created by Terraform, not by hand"). If
-- `odds.raw.*` or `odds.normalized` retention is set LONGER than 7 days, a
-- full-retention replay will write into compressed chunks. That is supported but
-- slow. Either keep raw-topic retention at or under 7 days, or raise this
-- threshold to match it in a follow-up migration. Do not discover the mismatch
-- during an incident.
--
-- `schedule_interval` is pinned at 6 hours rather than left to the default. It
-- happens to BE the default that timescaledb 2.29.1 derives today (verified: an
-- unqualified add_compression_policy produced 06:00:00), and pinning it means an
-- extension upgrade cannot silently change how often a background job wakes up
-- on a two-core machine.
--
--
-- =============================================================================
-- RETENTION: DELIBERATELY NONE. DO NOT ADD ONE.
-- =============================================================================
--
-- This is the decision most likely to be "corrected" by someone applying
-- time-series best practice from memory, so the reasoning is here in full.
--
-- 1. IT WOULD DELETE THE PRODUCT.
--    CLAUDE.md §3: line history is "the interesting dataset (CLV, steam
--    detection, book disagreement)". §6 lists "CLV tracking per user" and
--    "line movement charts from history" under the heading "Analytics -- the
--    differentiator". A retention policy is a scheduled job whose entire
--    function is to destroy that. CLV in particular is unbounded backwards by
--    construction: it compares a wager's placement price against the market's
--    closing price, and the leaderboard CLAUDE.md §6 wants is "a public
--    leaderboard on ROI and CLV" -- a lifetime statistic. Dropping chunks
--    silently truncates every user's history at the window boundary and the
--    number just quietly becomes wrong.
--
-- 2. THE DATA IS NOT PURCHASABLE, SO IT IS NOT REPLACEABLE.
--    ADR 0003 priced this exactly (scenario F, "historical backfill"): historical
--    odds cost 10x the live rate, and "one week of minute-resolution history
--    costs 86% of the entire 20K tier, and a season costs 3.1x the 100K tier."
--    Its conclusion is the architectural one this table exists to serve: "line
--    history is accumulated forward from our own live polling into the Timescale
--    hypertable [...] After one season of running, Sharpline owns a
--    line-movement dataset that would have cost hundreds of thousands of credits
--    to buy." A retention policy throws away an asset that cannot be re-bought
--    at this budget.
--
-- 3. THE DISK ARITHMETIC SAYS IT IS UNNECESSARY, NOT MERELY UNDESIRABLE --
--    WHICH IS THE ARGUMENT THAT ACTUALLY CLOSES IT.
--    Compressed at the measured 23.7 bytes/row:
--
--      operating tier   4.0 x 10^5 rows/day x 23.7 B  =  9.5 MB/day
--                                                     =  3.5 GB/year
--      Scenario D ceil  2.7 x 10^6 rows/day x 23.7 B  =  64 MB/day
--                                                     =  23 GB/year
--
--    Plus the 7-day uncompressed head (740 MB / 5.0 GB). The deploy target's
--    block volume is 200 GB. So a DECADE of line history at the tier this
--    project runs on is ~35 GB -- under a fifth of the volume. Even the $119
--    tier fits eight years.
--
--    A retention policy would therefore be trading the project's stated
--    differentiator for disk that is not scarce. There is no version of that
--    trade that is correct.
--
-- 4. CONSISTENT WITH THE PRECEDENT ALREADY ON DISK. 00007 makes the same call
--    for the other hypertable in this schema, in the same terms: "NO RETENTION
--    POLICY IS INSTALLED HERE, on purpose. Silently deleting audit history on a
--    timer is a policy decision with legal shape, not a schema default."
--
-- WHAT IS STILL AVAILABLE, AND THAT IS THE POINT. Making this a hypertable makes
-- `drop_chunks` and `add_retention_policy` POSSIBLE at any time, in one line,
-- without a data migration. 00003's append-only triggers mean `drop_chunks` on a
-- named time range is in fact the ONLY way any row can ever leave this table --
-- a deliberate administrative act on a whole time window, never a surgical
-- deletion of an inconvenient quote. Choosing to schedule it is a separate,
-- reviewed migration with a case of its own to make. Nothing here forecloses it;
-- this file simply declines to make that decision by default.
--
--
-- =============================================================================
-- CONTINUOUS AGGREGATES: DELIBERATELY NONE. ALSO NOT AN OVERSIGHT.
-- =============================================================================
--
-- The standard Timescale pattern is raw hypertable + retention + a continuous
-- aggregate holding the downsampled history. Four of the five reasons that
-- pattern exists do not hold here.
--
-- 1. IT IS SOMEONE ELSE'S DELIVERABLE. CLAUDE.md §3 lists, under "Apache Flink
--    -- phase 12, not before": "Line-movement aggregates -- tumbling windows
--    sunk into Timescale." That is this exact object, assigned to a later phase,
--    in a different technology, on purpose.
--
-- 2. IT WOULD CREATE A THIRD IMPLEMENTATION OF ONE NUMBER. CLAUDE.md §11 makes
--    phase 9 the Go reference implementation and §3 says phase 12's Flink jobs
--    are validated against it -- "same inputs, same outputs, or the Flink job is
--    wrong." A SQL continuous aggregate computing line-movement buckets would be
--    a third computation of the same quantity, refreshed on its own schedule,
--    with no reference telling you which of the three to trust when they
--    disagree. Two implementations with a stated authority is a validation
--    strategy; three is a bug farm.
--
-- 3. PHASE 2 DOES NOT KNOW THE AGGREGATE'S SHAPE, AND GUESSING IS WORSE THAN
--    WAITING. A continuous aggregate has to commit to a bucket width and a
--    specific set of aggregate expressions at creation time. Which? Open / high /
--    low / close of decimal_odds per selection per book? Line velocity for steam
--    detection? Max cross-book spread for arbitrage? Those are phase 9's
--    questions and phase 9 has not been designed. A materialised view that
--    guessed wrong is dead schema that costs a background refresh job forever.
--
-- 4. WITH NO RETENTION POLICY, DOWNSAMPLING HAS NO STORAGE PURPOSE. The pattern's
--    usual motivation is "keep aggregates, drop raw". Raw data here is kept
--    forever and costs 9.5 MB/day compressed, so an aggregate cannot save disk.
--    It could only save query latency -- for a query nobody has written, against
--    a table whose measured history reads are 0.02-0.07 ms.
--
-- 5. IT CANNOT BE VALIDATED TODAY. The database is empty and stays empty until
--    phase 3 (no mock data, ever). A continuous aggregate over zero rows can be
--    verified to EXIST and nothing more, which is precisely the kind of
--    unexercised machinery that is discovered to be wrong three phases later.
--
-- Same framing as retention: the hypertable makes `CREATE MATERIALIZED VIEW ...
-- WITH (timescaledb.continuous)` available whenever there is a measured query to
-- serve and a decided bucket width. Two conditions would make one correct: a
-- demonstrated slow chart query with a known bucket, or phase 12's Flink job
-- choosing to sink into a continuous aggregate rather than a plain table. Until
-- one of those exists, this file adds nothing.
--
-- =============================================================================

-- +goose Up

-- -----------------------------------------------------------------------------
-- Precondition: 00003 must have run. Asserted rather than assumed so that a
-- misordered or hand-edited migration set fails with one actionable sentence
-- instead of `relation "prices" does not exist`.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
DO $precondition$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM timescaledb_information.hypertables
         WHERE hypertable_name = 'prices'
    ) THEN
        RAISE EXCEPTION 'prices is not a hypertable; 00003_prices_hypertable must run before this migration'
            USING HINT = 'Compression settings and a compression policy are properties of a hypertable.';
    END IF;
END
$precondition$;
-- +goose StatementEnd

-- -----------------------------------------------------------------------------
-- Enable columnar compression.
--
-- segment_by = selection_id            (every product query leads with it)
-- order_by   = book_id, observed_at ↓  (book first for RLE and for cross-book
--                                       pruning; observed_at DESC so batches are
--                                       delta-delta encoded and carry min/max
--                                       time metadata)
--
-- The measurement tables in the header are the justification. Together the two
-- clauses also cover every column of prices_natural_key_idx, which is what makes
-- this ALTER legal on a hypertable carrying a unique index.
--
-- The classic `timescaledb.compress*` option names are used rather than the newer
-- columnstore spelling: they are what 2.29.1 accepts without a deprecation
-- notice, and they are the form every currently-published TimescaleDB reference
-- documents. (The extension does report the resulting job as a "Columnstore
-- Policy" internally; that is cosmetic.)
-- -----------------------------------------------------------------------------
ALTER TABLE prices SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'selection_id',
    timescaledb.compress_orderby   = 'book_id, observed_at DESC'
);

-- -----------------------------------------------------------------------------
-- The compression policy. 7 days; see the header for why the threshold is about
-- the out-of-order WRITE window rather than read latency.
-- -----------------------------------------------------------------------------
SELECT add_compression_policy(
    'prices',
    compress_after    => INTERVAL '7 days',
    schedule_interval => INTERVAL '6 hours'
);

-- -----------------------------------------------------------------------------
-- Postcondition. Asserts what was actually registered -- the settings landed on
-- the right columns in the right roles, and the background job exists -- so a
-- silently-misapplied policy is caught here and not in a capacity review a year
-- from now.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
DO $postcondition$
DECLARE
    v_seg     text;
    v_ord     text;
    v_jobs    int;
    v_after   text;
BEGIN
    SELECT string_agg(attname, ',' ORDER BY segmentby_column_index)
      INTO v_seg
      FROM timescaledb_information.compression_settings
     WHERE hypertable_name = 'prices' AND segmentby_column_index IS NOT NULL;

    SELECT string_agg(attname || CASE WHEN orderby_asc THEN ' ASC' ELSE ' DESC' END,
                      ',' ORDER BY orderby_column_index)
      INTO v_ord
      FROM timescaledb_information.compression_settings
     WHERE hypertable_name = 'prices' AND orderby_column_index IS NOT NULL;

    IF v_seg IS DISTINCT FROM 'selection_id' THEN
        RAISE EXCEPTION 'prices compress_segmentby is [%], expected [selection_id]', coalesce(v_seg, '<none>')
            USING HINT = 'See the measured segment_by comparison in migrations/00004_timescale_policies.sql.';
    END IF;

    IF v_ord IS DISTINCT FROM 'book_id ASC,observed_at DESC' THEN
        RAISE EXCEPTION 'prices compress_orderby is [%], expected [book_id ASC,observed_at DESC]', coalesce(v_ord, '<none>');
    END IF;

    SELECT count(*), max(config ->> 'compress_after')
      INTO v_jobs, v_after
      FROM timescaledb_information.jobs
     WHERE hypertable_name = 'prices' AND proc_name = 'policy_compression';

    IF v_jobs <> 1 THEN
        RAISE EXCEPTION 'expected exactly 1 compression policy job on prices, found %', v_jobs;
    END IF;

    -- Deliberately asserted: there must be NO retention policy and NO
    -- continuous aggregate on this hypertable. Both absences are arguments in
    -- this file's header, and an assertion is how an argument survives being
    -- read. Adding either must be a reviewed migration that also updates the
    -- reasoning above -- not a line someone slips in because a dashboard looked
    -- slow.
    IF EXISTS (SELECT 1 FROM timescaledb_information.jobs
                WHERE hypertable_name = 'prices' AND proc_name = 'policy_retention') THEN
        RAISE EXCEPTION 'a retention policy exists on prices'
            USING HINT = 'CLAUDE.md sections 3 and 6 make line history the differentiator, and the disk '
                         'arithmetic in this migration shows a decade of it fits in under a fifth of the '
                         'deploy volume. Removing history is never the cheaper option here.';
    END IF;

    IF EXISTS (SELECT 1 FROM timescaledb_information.continuous_aggregates
                WHERE hypertable_name = 'prices') THEN
        RAISE EXCEPTION 'a continuous aggregate exists on prices'
            USING HINT = 'CLAUDE.md section 3 assigns line-movement aggregates to the phase 12 Flink SQL '
                         'jobs, validated against the phase 9 Go reference implementation.';
    END IF;

    RAISE NOTICE 'prices: compression segmentby=[%] orderby=[%], policy compress_after=%, no retention, no continuous aggregate',
        v_seg, v_ord, v_after;
END
$postcondition$;
-- +goose StatementEnd

-- +goose Down

-- -----------------------------------------------------------------------------
-- The order of these three statements is MANDATORY, not stylistic. Verified on
-- the pinned image by getting it wrong first:
--
--   1. Remove the policy BEFORE decompressing. The policy is a live background
--      job; leaving it in place while step 2 runs means it can re-compress a
--      chunk that has just been decompressed, and step 3 then fails.
--
--   2. Decompress EVERY chunk. This is not defensive -- it is required. With any
--      chunk still compressed, step 3 fails outright:
--
--          ERROR:  cannot disable columnstore on hypertable with columnstore chunks
--
--      `if_compressed => TRUE` makes the call a no-op on chunks that were never
--      compressed, so this is correct whether the policy ever ran or not, and
--      correct on the empty table that `make migrate-dry-run` rolls back (where
--      show_chunks returns no rows and the statement affects nothing).
--
--      Note that decompression re-INSERTs rows into the plain chunk, which
--      00003's append-only guards permit -- INSERT is the one operation they do
--      not block. Verified end to end.
--
--   3. Only now can the compression settings be removed. This also deletes the
--      rows in timescaledb_information.compression_settings; verified to leave
--      zero settings and zero jobs behind.
--
-- After this Down the hypertable, its chunks, its index and its data are exactly
-- as 00003 left them. Nothing this migration did not create is touched -- in
-- particular the hypertable itself is NOT dropped here, for the same reason
-- 00001 does not drop the timescaledb extension: a Down that destroys what its
-- Up did not create is a second forward migration pointed backwards.
-- -----------------------------------------------------------------------------
SELECT remove_compression_policy('prices', if_exists => TRUE);

-- +goose StatementBegin
DO $decompress$
DECLARE
    v_chunk regclass;
BEGIN
    FOR v_chunk IN SELECT show_chunks('prices') LOOP
        PERFORM decompress_chunk(v_chunk, if_compressed => TRUE);
    END LOOP;
END
$decompress$;
-- +goose StatementEnd

ALTER TABLE prices SET (timescaledb.compress = FALSE);
