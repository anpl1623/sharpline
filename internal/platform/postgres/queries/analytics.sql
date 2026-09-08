/*
 * ============================================================================
 * analytics -- phase 9's writes and reads: signals, CLV, and the leaderboard
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumers, and each one owns a disjoint half of this file:
 *
 *   `pricer`  writes ev_signals, arbitrage_signals (+ legs) and steam_signals.
 *             CLAUDE.md §11 phase 9 puts the signals stage in the pricer because
 *             it already consumes the market stream and already computes EV and
 *             arbitrage; a seventh binary to host analytics would contradict
 *             §3's service table, which has exactly six rows.
 *
 *   `settle`  writes wager_leg_clv, and reads MarketSnapshotAtBookAsOf twice per
 *             graded leg to build the two odds.FairMarketSnapshot values
 *             odds.EvaluateCLV compares. odds/clv.go names settle as the writer
 *             in as many words.
 *
 *   `api`     reads everything, and writes nothing here.
 *
 * -----------------------------------------------------------------------------
 * EVERY WRITE IS AN UPSERT, AND THAT IS A CONTRACT WITH PHASE 12
 * -----------------------------------------------------------------------------
 * Migration 00009's header has the full argument; the operational summary is:
 * signals are DERIVED, so unlike `prices` and `ledger_entries` they may be
 * recomputed, and recomputation must be idempotent because the pricer and settle
 * are Kafka consumers with at-least-once delivery. A rebalance, a deliberate
 * offset reset, or a replay of `price.computed` re-presents records that have
 * already been written.
 *
 * So each write below names an explicit conflict target, and every one of those
 * targets is a PURE FUNCTION OF THE INPUT DATA -- no clock reading, no
 * surrogate, no sequence:
 *
 *   ev_signals         (selection_id, book_id, quote_observed_at)
 *   steam_signals      (market_id, selection_id, window_start, window_end)
 *   arbitrage_signals  (market_id, observed_at, legs_fingerprint)
 *   wager_leg_clv      (leg_id)
 *
 * DO UPDATE rather than DO NOTHING, which is where this file differs from
 * prices.sql. `prices` is immutable by trigger, so a redelivery must be a no-op.
 * A signal is not: if the detector was corrected between the first computation
 * and the replay, the replay is the CORRECTION and must land. DO NOTHING would
 * silently preserve the wrong answer and there would be no way to tell, because
 * the row would still exist and still look plausible.
 *
 * Phase 12's Flink SQL jobs must key their upserts the same four ways. A job
 * that keys steam by (market_id, window_end) alone collapses the two sides of one
 * move into a single row and silently drops half the findings.
 *
 * -----------------------------------------------------------------------------
 * PAGINATION: KEYSET, ALL-DESCENDING
 * -----------------------------------------------------------------------------
 * api.sql's header has the argument for keyset over OFFSET and it applies with
 * more force here, because the pricer writes these tables continuously rather
 * than merely often.
 *
 * The one thing this file does differently is that EVERY component of every
 * ordering is DESC, including the identifier tie-breakers, which read oddly until
 * you know why. A keyset predicate is a ROW-VALUE comparison
 * `(a, b, c) < (@a, @b, @c)`, and PostgreSQL plans that as a single index range
 * only when every component sorts the same way. `ORDER BY ev DESC, selection_id
 * ASC` cannot be written as a row comparison at all, and its expansion into
 * `a < x OR (a = x AND b > y)` is exactly the form api.sql identifies as "where
 * off-by-one duplicate-row bugs live". The tie-breakers carry no meaning of their
 * own -- they exist to make the ordering total -- so descending serves as well as
 * ascending and keeps the predicate expressible.
 *
 * -----------------------------------------------------------------------------
 * TWO KINDS OF PREDICATE, AND WHY ONLY ONE OF THEM GETS ITS OWN STATEMENT
 * -----------------------------------------------------------------------------
 * api.sql establishes that `(@x IS NULL OR col = @x)` is not sargable and defeats
 * the index the design depends on, and answers that with a statement per shape.
 * That rule is about predicates that must DRIVE an index. It is not a blanket ban
 * on optional filters, and this file distinguishes the two:
 *
 *   INDEX-DRIVING, so a separate statement:
 *       league_id   -- a cross-league query cannot use an index led by league_id
 *
 *   RESIDUAL, so an in-statement filter:
 *       book_id, market_type  -- both low cardinality, both applied after the
 *       ranking column and the time bound have already narrowed the candidate
 *       set to a page's worth. Giving each its own statement would multiply the
 *       query count by four for no plan change.
 *
 * Residual filters use the EMPTY-ARRAY idiom, `cardinality(@x::TEXT[]) = 0 OR
 * col = ANY(@x::TEXT[])`, rather than a nullable scalar. Two reasons: sqlc types
 * an explicitly cast array parameter from the CAST (sqlc.yaml note 3), so the Go
 * signature is a predictable []string rather than a pgtype; and the board's
 * filter controls are multi-select, so the array is the shape the API wants
 * anyway. Passing an empty slice means "no filter".
 *
 * -----------------------------------------------------------------------------
 * THE TIME BOUND ON A READ IS NOT OPTIONAL
 * -----------------------------------------------------------------------------
 * prices.sql says this about `prices` and it is equally true of ev_signals and
 * steam_signals, which are hypertables with no retention policy for the same
 * reason. Every read of either takes a REQUIRED lower bound on its partitioning
 * column. There is no unbounded variant to reach for by accident.
 * ============================================================================
 */


-- ============================================================================
-- WRITES -- pricer
-- ============================================================================

-- Record one +EV finding.
--
-- The conflict target is the natural key: one quote -- one (selection, book,
-- provider instant) -- yields at most one finding. DO UPDATE, not DO NOTHING:
-- see the header. Everything that can legitimately differ between two
-- computations of the same quote is updated; nothing that is part of the key is,
-- because it cannot have changed.
--
-- created_at is deliberately absent from the SET list. It records when the row
-- FIRST appeared, which is a different fact from updated_at and is the only way
-- to tell an original computation from a recomputation after the fact. The
-- analytics_set_updated_at trigger stamps updated_at.
--
-- :exec rather than :one. The caller has the natural key already -- it built it
-- -- so there is nothing to return, and RETURNING on an upsert against a
-- hypertable chunk costs a round trip's worth of tuple for no information.
--
-- name: UpsertEVSignal :exec
INSERT INTO ev_signals (
    selection_id, market_id, market_type, league_id,
    book_id, reference_book_id, devig_method,
    offered_decimal, offered_implied, line,
    fair_probability, fair_decimal,
    expected_value, expected_value_percent,
    edge, edge_percent,
    kelly, fractional_kelly, kelly_fraction,
    quote_observed_at, quote_age_seconds,
    threshold_ev_percent, max_quote_age_seconds,
    detected_at
) VALUES (
    @selection_id, @market_id, @market_type, @league_id,
    @book_id, @reference_book_id, @devig_method,
    @offered_decimal, @offered_implied, @line,
    @fair_probability, @fair_decimal,
    @expected_value, @expected_value_percent,
    @edge, @edge_percent,
    @kelly, @fractional_kelly, @kelly_fraction,
    @quote_observed_at, @quote_age_seconds,
    @threshold_ev_percent, @max_quote_age_seconds,
    @detected_at
)
ON CONFLICT (selection_id, book_id, quote_observed_at) DO UPDATE
SET market_id              = EXCLUDED.market_id,
    market_type            = EXCLUDED.market_type,
    league_id              = EXCLUDED.league_id,
    reference_book_id      = EXCLUDED.reference_book_id,
    devig_method           = EXCLUDED.devig_method,
    offered_decimal        = EXCLUDED.offered_decimal,
    offered_implied        = EXCLUDED.offered_implied,
    line                   = EXCLUDED.line,
    fair_probability       = EXCLUDED.fair_probability,
    fair_decimal           = EXCLUDED.fair_decimal,
    expected_value         = EXCLUDED.expected_value,
    expected_value_percent = EXCLUDED.expected_value_percent,
    edge                   = EXCLUDED.edge,
    edge_percent           = EXCLUDED.edge_percent,
    kelly                  = EXCLUDED.kelly,
    fractional_kelly       = EXCLUDED.fractional_kelly,
    kelly_fraction         = EXCLUDED.kelly_fraction,
    quote_age_seconds      = EXCLUDED.quote_age_seconds,
    threshold_ev_percent   = EXCLUDED.threshold_ev_percent,
    max_quote_age_seconds  = EXCLUDED.max_quote_age_seconds,
    detected_at            = EXCLUDED.detected_at;


-- Record one arbitrage finding and return its surrogate id, which the caller
-- needs to write the legs.
--
-- THE FINGERPRINT IS THE CALLER'S OBLIGATION AND IT IS LOAD-BEARING.
-- (market_id, observed_at) is not unique: one market with several books can yield
-- more than one under-round combination at a single instant. legs_fingerprint
-- separates them, and it must be a PURE FUNCTION OF THE LEG SET with the
-- following properties, because phase 12 has to reproduce it:
--
--   * over the tuple (selection_id, book_id, decimal_odds, line) of every leg;
--   * legs sorted by selection_id before hashing, so Go map iteration order and
--     Flink's arbitrary collect order cannot change the digest;
--   * decimal_odds and line formatted with a fixed, lossless representation
--     (strconv.FormatFloat 'g', -1, 64 round-trips a float64 exactly);
--   * lowercase hex, 16 to 64 characters -- the column's CHECK enforces the
--     shape but cannot enforce the function;
--   * NO clock, NO random, NO detector version in the input.
--
-- Get this wrong in one direction and replay duplicates every finding; wrong in
-- the other and two genuinely different findings collapse into one.
--
-- :one because the id is a server-generated UUID the caller cannot predict. On
-- the update path RETURNING gives back the EXISTING id, which is what makes
-- replace-the-legs work: the caller deletes and reinserts under the same id
-- rather than orphaning a leg set.
--
-- name: UpsertArbitrageSignal :one
INSERT INTO arbitrage_signals (
    market_id, market_type, league_id, line,
    selection_count, implied_sum, return_fraction, distinct_books,
    observed_spread_seconds, oldest_leg_age_seconds, observed_at,
    legs_fingerprint,
    max_leg_age_seconds, max_observed_spread_seconds,
    detected_at
) VALUES (
    @market_id, @market_type, @league_id, @line,
    @selection_count, @implied_sum, @return_fraction, @distinct_books,
    @observed_spread_seconds, @oldest_leg_age_seconds, @observed_at,
    @legs_fingerprint,
    @max_leg_age_seconds, @max_observed_spread_seconds,
    @detected_at
)
ON CONFLICT ON CONSTRAINT arbitrage_signals_natural_key DO UPDATE
SET market_type                 = EXCLUDED.market_type,
    league_id                   = EXCLUDED.league_id,
    line                        = EXCLUDED.line,
    selection_count             = EXCLUDED.selection_count,
    implied_sum                 = EXCLUDED.implied_sum,
    return_fraction             = EXCLUDED.return_fraction,
    distinct_books              = EXCLUDED.distinct_books,
    observed_spread_seconds     = EXCLUDED.observed_spread_seconds,
    oldest_leg_age_seconds      = EXCLUDED.oldest_leg_age_seconds,
    max_leg_age_seconds         = EXCLUDED.max_leg_age_seconds,
    max_observed_spread_seconds = EXCLUDED.max_observed_spread_seconds,
    detected_at                 = EXCLUDED.detected_at
RETURNING id;


-- Clear one finding's legs so the caller can write the current set.
--
-- The leg set is REPLACED WHOLESALE rather than upserted leg by leg, and the
-- distinction matters on the update path: a recomputation that found a BETTER
-- combination at the same instant has a different leg set, possibly with fewer
-- legs. Upserting leg-by-leg would leave the surplus rows behind, and the finding
-- would then describe an arbitrage whose stake fractions no longer sum to 1.
--
-- Delete-then-insert inside the caller's transaction, not a MERGE: the two
-- statements are simpler to reason about than a MERGE's three branches, and this
-- runs at most a handful of rows.
--
-- name: DeleteArbitrageSignalLegs :exec
DELETE FROM arbitrage_signal_legs
 WHERE signal_id = @signal_id;


-- One outcome of one finding.
--
-- ON CONFLICT DO UPDATE rather than a plain INSERT even though
-- DeleteArbitrageSignalLegs has just cleared the set, because the pair is only
-- atomic if the caller wraps both in one transaction and this statement must
-- still be correct if it does not. Cheap insurance on a table that sees a handful
-- of rows per finding.
--
-- name: UpsertArbitrageSignalLeg :exec
INSERT INTO arbitrage_signal_legs (
    signal_id, leg_index, selection_id, role, book_id,
    decimal_odds, line, stake_fraction, observed_at, age_seconds
) VALUES (
    @signal_id, @leg_index, @selection_id, @role, @book_id,
    @decimal_odds, @line, @stake_fraction, @observed_at, @age_seconds
)
ON CONFLICT (signal_id, leg_index) DO UPDATE
SET selection_id   = EXCLUDED.selection_id,
    role           = EXCLUDED.role,
    book_id        = EXCLUDED.book_id,
    decimal_odds   = EXCLUDED.decimal_odds,
    line           = EXCLUDED.line,
    stake_fraction = EXCLUDED.stake_fraction,
    observed_at    = EXCLUDED.observed_at,
    age_seconds    = EXCLUDED.age_seconds;


-- Record one steam detection.
--
-- The conflict target carries BOTH window edges, because a hopping window
-- overlaps its neighbour: (market, side, [t, t+L)) and (market, side, [t+h, t+h+L))
-- are two findings over overlapping data, not a duplicate of one. Phase 12's HOP
-- window emits exactly the same pairs.
--
-- `followers` is JSONB. The Go side marshals a slice of
-- {book_id, moved_at, lag_seconds, delta_probability}, ordered by lag ascending;
-- migration 00009 documents the element shape and the fact that book_id inside a
-- JSON document is NOT foreign-key enforced. The database checks that the value
-- is an array and that follower_count equals its length, and nothing more --
-- the element shape is the writer's obligation and phase 12's contract.
--
-- name: UpsertSteamSignal :exec
INSERT INTO steam_signals (
    market_id, market_type, league_id, selection_id,
    window_start, window_end, window_seconds, hop_seconds,
    direction, delta_probability, magnitude_probability_points,
    velocity_probability_per_minute, devig_method,
    lead_book_id, lead_moved_at, followers, follower_count, participating_books,
    cross_book_correlation,
    threshold_velocity, threshold_magnitude, threshold_correlation,
    min_followers, max_follower_lag_seconds,
    detected_at
) VALUES (
    @market_id, @market_type, @league_id, @selection_id,
    @window_start, @window_end, @window_seconds, @hop_seconds,
    @direction, @delta_probability, @magnitude_probability_points,
    @velocity_probability_per_minute, @devig_method,
    @lead_book_id, @lead_moved_at, @followers, @follower_count, @participating_books,
    @cross_book_correlation,
    @threshold_velocity, @threshold_magnitude, @threshold_correlation,
    @min_followers, @max_follower_lag_seconds,
    @detected_at
)
ON CONFLICT (market_id, selection_id, window_start, window_end) DO UPDATE
SET market_type                     = EXCLUDED.market_type,
    league_id                       = EXCLUDED.league_id,
    window_seconds                  = EXCLUDED.window_seconds,
    hop_seconds                     = EXCLUDED.hop_seconds,
    direction                       = EXCLUDED.direction,
    delta_probability               = EXCLUDED.delta_probability,
    magnitude_probability_points    = EXCLUDED.magnitude_probability_points,
    velocity_probability_per_minute = EXCLUDED.velocity_probability_per_minute,
    devig_method                    = EXCLUDED.devig_method,
    lead_book_id                    = EXCLUDED.lead_book_id,
    lead_moved_at                   = EXCLUDED.lead_moved_at,
    followers                       = EXCLUDED.followers,
    follower_count                  = EXCLUDED.follower_count,
    participating_books             = EXCLUDED.participating_books,
    cross_book_correlation          = EXCLUDED.cross_book_correlation,
    threshold_velocity              = EXCLUDED.threshold_velocity,
    threshold_magnitude             = EXCLUDED.threshold_magnitude,
    threshold_correlation           = EXCLUDED.threshold_correlation,
    min_followers                   = EXCLUDED.min_followers,
    max_follower_lag_seconds        = EXCLUDED.max_follower_lag_seconds,
    detected_at                     = EXCLUDED.detected_at;


-- ============================================================================
-- WRITES -- settle
-- ============================================================================

-- Record one graded leg's closing line value: the persisted form of
-- odds.CLVResult.
--
-- Conflict on the PRIMARY KEY, which is leg_id alone. Idempotency is free here in
-- a way it is not for the three signal tables: a leg has exactly one placement
-- price and exactly one close, so there is no fingerprint to compute and no
-- compound key to get wrong.
--
-- WRITE THIS ROW ONLY WHEN odds.EvaluateCLV (or EvaluateCLVAcrossLineMove)
-- SUCCEEDED. Migration 00009's header enumerates the four failure modes that must
-- produce NO ROW rather than a row of nulls -- an incomplete close, a changed
-- outcome set, a close preceding the take, and a market that was suspended
-- through every candidate closing quote. "We could not measure it" and "it
-- measured zero" must not share a shape, or the leaderboard cannot tell them
-- apart.
--
-- name: UpsertWagerLegCLV :exec
INSERT INTO wager_leg_clv (
    leg_id, wager_id, user_id,
    market_id, market_type, selection_id, league_id,
    taken_book_id, closing_book_id, devig_method,
    taken_line, closing_line, taken_at, closed_at,
    taken_fair, closing_fair, taken_price, closing_price,
    probability_clv, percent_clv, magnitude,
    beat_close, line_moved, leg_status, voided,
    graded_at, computed_at
) VALUES (
    @leg_id, @wager_id, @user_id,
    @market_id, @market_type, @selection_id, @league_id,
    @taken_book_id, @closing_book_id, @devig_method,
    @taken_line, @closing_line, @taken_at, @closed_at,
    @taken_fair, @closing_fair, @taken_price, @closing_price,
    @probability_clv, @percent_clv, @magnitude,
    @beat_close, @line_moved, @leg_status, @voided,
    @graded_at, @computed_at
)
ON CONFLICT (leg_id) DO UPDATE
SET closing_book_id  = EXCLUDED.closing_book_id,
    devig_method     = EXCLUDED.devig_method,
    taken_line       = EXCLUDED.taken_line,
    closing_line     = EXCLUDED.closing_line,
    taken_at         = EXCLUDED.taken_at,
    closed_at        = EXCLUDED.closed_at,
    taken_fair       = EXCLUDED.taken_fair,
    closing_fair     = EXCLUDED.closing_fair,
    taken_price      = EXCLUDED.taken_price,
    closing_price    = EXCLUDED.closing_price,
    probability_clv  = EXCLUDED.probability_clv,
    percent_clv      = EXCLUDED.percent_clv,
    magnitude        = EXCLUDED.magnitude,
    beat_close       = EXCLUDED.beat_close,
    line_moved       = EXCLUDED.line_moved,
    leg_status       = EXCLUDED.leg_status,
    voided           = EXCLUDED.voided,
    graded_at        = EXCLUDED.graded_at,
    computed_at      = EXCLUDED.computed_at;


-- ============================================================================
-- THE CLOSING PRICE
-- ============================================================================
--
-- This is the hard query in phase 9 and it is worth reading the whole comment
-- before using it, because most of the correctness is in the predicates rather
-- than in the shape.
--
-- -----------------------------------------------------------------------------
-- WHAT "THE CLOSE" IS, PRECISELY. PHASE 12 MUST REPRODUCE THIS EXACTLY.
-- -----------------------------------------------------------------------------
-- The closing snapshot of market M at book B is:
--
--     for EVERY selection s of M:
--         the price at B for s with the GREATEST observed_at satisfying
--             observed_at <= as_of
--             observed_at >  not_before
--             observed_at not inside any suspension episode of M
--
-- and the snapshot is VALID only if every selection of M yielded such a price.
--
-- Five things in that definition are decisions, not mechanics:
--
--   1. AS_OF IS events.scheduled_start, NOT THE ACTUAL KICKOFF and not the
--      instant the market's status changed. Use GetMarketClosingInstant below to
--      obtain it. scheduled_start is the only instant that is knowable before the
--      event, stable across replays, and identical in Go and in Flink -- an
--      actual-kickoff timestamp derived from the live clock feed is none of those
--      three. It is also what "closing line" means in the literature.
--
--   2. NOT_BEFORE IS REQUIRED, and is a lookback window as well as a chunk
--      filter. `prices` is partitioned on observed_at with 12-hour chunks and
--      migration 00004 installs NO retention policy, so an unbounded lower bound
--      makes the planner consult the index on every chunk that has ever existed
--      -- prices.sql calls this the sharpest edge on that table. It is also a
--      SEMANTIC bound: a quote from six days before kickoff is not a closing
--      line, it is a market nobody has priced since. Callers pass
--      (scheduled_start - closing_lookback); the lookback is a declared
--      parameter of the CLV pass, not a magic number.
--
--   3. ONE BOOK, THE WHOLE OUTCOME SET. Devigging is defined over a complete
--      market -- odds.NewFairMarketSnapshot rejects probabilities that do not sum
--      to 1 within CLVDevigTolerance, which is precisely how it "mechanically
--      refuses vigged input" -- so a snapshot assembled from the best price at
--      each book is not devigable and is not a close. Hence one @book_id, and
--      hence the completeness rule below.
--
--   4. A SUSPENDED MARKET'S STALE QUOTE IS NOT A CLOSE. This is the predicate
--      that makes the query non-obvious. When a market is suspended the books
--      stop moving it, so the last quote before kickoff may be a frozen price
--      from an hour earlier that nobody could have bet. The NOT EXISTS excludes
--      any quote observed inside a suspension episode:
--
--          suspended_at <= observed_at < COALESCE(lifted_at, +infinity)
--
--      Half-open, so the quote that arrives at the exact instant a suspension is
--      lifted counts and the one at the instant it begins does not.
--
--      A market SUSPENDED AND REOPENED before the start is handled by this alone
--      and needs no special case: quotes during the episode are excluded, quotes
--      after the lift are eligible, and the last eligible one wins. A market
--      suspended and NEVER reopened has an episode with lifted_at IS NULL, so
--      every quote from suspended_at onwards is excluded and the query falls back
--      to the last quote before the suspension began -- which is, correctly, the
--      last price at which the market was actually open. If that falls outside
--      not_before, the selection yields nothing and the snapshot is incomplete,
--      which is also correct: there is no close.
--
--      Note that markets.status is NOT consulted, deliberately. It is current
--      state and says nothing about what was true at as_of, whereas
--      market_suspensions is the episode history. 00007's
--      market_suspensions_one_open_idx guarantees at most one open episode per
--      market, so the anti-join cannot be confused by overlapping episodes.
--
--   5. TIES. Two prices for one selection at one book with the same observed_at
--      cannot exist: prices_natural_key_idx is UNIQUE on
--      (selection_id, book_id, observed_at). There is no tie-break to specify,
--      and phase 12 needs none.
--
-- -----------------------------------------------------------------------------
-- COMPLETENESS IS THE CALLER'S CHECK, AND IT IS NOT OPTIONAL
-- -----------------------------------------------------------------------------
-- The lateral is an INNER join, so a selection with no eligible quote produces NO
-- ROW. `market_selections` is returned on every row and is the count of
-- selections the market actually has.
--
--     len(rows) < market_selections  ==>  THE SNAPSHOT IS INCOMPLETE. Discard it.
--                                         Do not devig the subset, do not write a
--                                         CLV row, do not fall back to another
--                                         book.
--
-- Devigging a partial outcome set produces probabilities that sum to less than 1
-- and are wrong by the missing selection's entire mass -- and
-- NewFairMarketSnapshot will reject them, so the failure is loud, but the caller
-- should not have got that far. A row count of zero means the market had no
-- eligible quotes at all.
--
-- The same statement builds the TAKEN snapshot: call it with the leg's own
-- price_book_id and price_observed_at as (@book_id, @as_of). Applying the
-- suspension exclusion to the taken side is harmless -- a leg cannot be placed on
-- a suspended market -- and one statement for both sides means the two snapshots
-- odds.EvaluateCLV compares are built by identical rules, which is the property
-- that matters.
--
-- -----------------------------------------------------------------------------
-- WHICH INDEX SERVES IT
-- -----------------------------------------------------------------------------
-- prices_natural_key_idx (selection_id, book_id, observed_at DESC) serves the
-- lateral directly, and this is the fourth read pattern migration 00003 lists for
-- it verbatim: "CLV -- WHERE selection_id = $1 AND book_id = $2 AND observed_at
-- <= $closing ORDER BY observed_at DESC LIMIT 1 -- two equalities plus a backward
-- walk from the boundary: one index probe." One probe per selection, so a
-- three-way moneyline is three probes.
--
-- The outer scan is selections_market_idx (market_id) from 00002.
--
-- The anti-join is served by market_suspensions_market_idx
-- (market_id, suspended_at DESC) from 00007, and is evaluated per candidate row
-- inside the lateral. Honest note on the one case where that costs something: a
-- market suspended for a long stretch ending at kickoff makes the backward walk
-- traverse every quote in the suspended stretch before finding an eligible one.
-- Bounded by not_before, and suspensions are rare and short, so this is left as
-- the simple form rather than pre-computing eligible time ranges into a CTE.
-- If measurement ever disagrees, that CTE is the fix and it is a rewrite of this
-- statement alone.
--
-- name: MarketSnapshotAtBookAsOf :many
SELECT s.id            AS selection_id,
       s.role          AS role,
       q.decimal_odds  AS decimal_odds,
       q.line          AS line,
       q.observed_at   AS observed_at,
       (SELECT count(*) FROM selections t WHERE t.market_id = @market_id)::BIGINT
                       AS market_selections
  FROM selections s
  CROSS JOIN LATERAL (
      SELECT p.decimal_odds, p.line, p.observed_at
        FROM prices p
       WHERE p.selection_id = s.id
         AND p.book_id      = @book_id
         AND p.observed_at <= @as_of
         AND p.observed_at  > @not_before
         AND NOT EXISTS (
             SELECT 1
               FROM market_suspensions ms
              WHERE ms.market_id  = @market_id
                AND ms.suspended_at <= p.observed_at
                AND (ms.lifted_at IS NULL OR p.observed_at < ms.lifted_at)
         )
       ORDER BY p.observed_at DESC
       LIMIT 1
  ) q
 WHERE s.market_id = @market_id
 ORDER BY s.id;


-- The instant a market closes, plus the identity columns settle needs to stamp
-- onto the CLV row.
--
-- One statement rather than making the caller walk markets -> events -> leagues,
-- because every graded leg needs all four values and a leg is graded one at a
-- time.
--
-- events.scheduled_start is the close (see the definition above). It is returned
-- alongside the event's status so the caller can notice the pathological case
-- where a market is being graded for an event that never started; there is no
-- CLV for a postponed fixture and settle should not invent one.
--
-- name: GetMarketClosingInstant :one
SELECT m.id           AS market_id,
       m.type         AS market_type,
       m.status       AS market_status,
       e.id           AS event_id,
       e.status       AS event_status,
       e.scheduled_start,
       l.id           AS league_id
  FROM markets m
  JOIN events  e ON e.id = m.event_id
  JOIN leagues l ON l.id = e.league_id
 WHERE m.id = @market_id;


-- The CLV work queue: graded legs that have no CLV row yet.
--
-- settle grades a leg and then measures it, and the two are separate steps
-- because the close is not knowable until the event starts while grading is not
-- possible until it ends. This query is what makes the second step restartable:
-- a settle instance that crashed between grading and measuring finds its work
-- here on restart, and so does one whose measurement failed for a reason that has
-- since been fixed (a backfilled price, a lifted suspension recorded late).
--
-- It is a LEFT JOIN anti-join rather than NOT IN, so a NULL cannot make the
-- predicate unknown for every row.
--
-- Bounded by graded_at, and ordered oldest-first: the queue is drained in the
-- order the legs became measurable, so a backlog does not starve the oldest work.
-- The bound is REQUIRED for the usual reason plus one specific to this query --
-- without it, every leg ever graded and legitimately unmeasurable (an incomplete
-- close that will never become complete) is re-attempted on every pass, forever.
--
-- name: ListGradedLegsAwaitingCLV :many
SELECT lg.id                AS leg_id,
       lg.wager_id,
       w.user_id,
       lg.event_id,
       lg.market_id,
       lg.market_type,
       lg.selection_id,
       lg.price_book_id,
       lg.price_decimal,
       lg.price_line,
       lg.price_observed_at,
       lg.status            AS leg_status,
       lg.graded_at
  FROM legs lg
  JOIN wagers w        ON w.id = lg.wager_id
  LEFT JOIN wager_leg_clv c ON c.leg_id = lg.id
 WHERE lg.status <> 'pending'
   AND lg.graded_at >= @graded_from
   AND lg.graded_at <  @graded_to
   AND c.leg_id IS NULL
 ORDER BY lg.graded_at, lg.id
 LIMIT @row_limit;


-- ============================================================================
-- READS -- the +EV finder
-- ============================================================================
--
-- Four statements: {cross-league, one league} x {first page, after cursor}. The
-- league split is index-driving and therefore a separate statement; book and
-- market-type filters are residual and therefore in-statement. The header
-- explains the distinction and the empty-array idiom.
--
-- @min_ev_percent is the caller's threshold and is applied ON TOP of the
-- threshold that was in force when each row was written. The two are different
-- things and both matter: threshold_ev_percent says what the detector was
-- configured to emit, @min_ev_percent says what this reader wants to see. A
-- reader asking for less than the detector emitted simply gets everything the
-- detector emitted; there is no way to recover findings that were never written.
--
-- @observed_after is REQUIRED. ev_signals is a hypertable with no retention
-- policy.
--
-- name: ListEVSignalsFirstPage :many
SELECT selection_id, market_id, market_type, league_id,
       book_id, reference_book_id, devig_method,
       offered_decimal, offered_implied, line,
       fair_probability, fair_decimal,
       expected_value, expected_value_percent,
       edge, edge_percent,
       kelly, fractional_kelly, kelly_fraction,
       quote_observed_at, quote_age_seconds,
       threshold_ev_percent, max_quote_age_seconds,
       detected_at
  FROM ev_signals
 WHERE quote_observed_at > @observed_after
   AND expected_value_percent >= @min_ev_percent
   AND (cardinality(@book_ids::TEXT[]) = 0 OR book_id = ANY(@book_ids::TEXT[]))
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
 ORDER BY expected_value_percent DESC, quote_observed_at DESC,
          selection_id DESC, book_id DESC
 LIMIT @row_limit;


-- Continued from a cursor.
--
-- `observed_after` and `min_ev_percent` are repeated from the first page and MUST
-- be the same values, for api.sql's stated reason: they are the window the cursor
-- was minted inside, and changing either between pages silently changes the set
-- being paged. The API binds them into the cursor and rejects a cursor presented
-- against a different window.
--
-- The four-column row comparison is a single index range on
-- ev_signals_rank_idx. It is total because (selection_id, book_id,
-- quote_observed_at) is unique.
--
-- name: ListEVSignalsAfterCursor :many
SELECT selection_id, market_id, market_type, league_id,
       book_id, reference_book_id, devig_method,
       offered_decimal, offered_implied, line,
       fair_probability, fair_decimal,
       expected_value, expected_value_percent,
       edge, edge_percent,
       kelly, fractional_kelly, kelly_fraction,
       quote_observed_at, quote_age_seconds,
       threshold_ev_percent, max_quote_age_seconds,
       detected_at
  FROM ev_signals
 WHERE quote_observed_at > @observed_after
   AND expected_value_percent >= @min_ev_percent
   AND (cardinality(@book_ids::TEXT[]) = 0 OR book_id = ANY(@book_ids::TEXT[]))
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
   AND (expected_value_percent, quote_observed_at, selection_id, book_id)
       < (@after_ev_percent::DOUBLE PRECISION, @after_quote_observed_at::TIMESTAMPTZ,
          @after_selection_id::TEXT, @after_book_id::TEXT)
 ORDER BY expected_value_percent DESC, quote_observed_at DESC,
          selection_id DESC, book_id DESC
 LIMIT @row_limit;


-- The same board within one league. Served by ev_signals_league_rank_idx; a
-- cross-league query cannot use an index led by league_id, which is why this is a
-- separate statement rather than a nullable filter on the pair above.
--
-- name: ListLeagueEVSignalsFirstPage :many
SELECT selection_id, market_id, market_type, league_id,
       book_id, reference_book_id, devig_method,
       offered_decimal, offered_implied, line,
       fair_probability, fair_decimal,
       expected_value, expected_value_percent,
       edge, edge_percent,
       kelly, fractional_kelly, kelly_fraction,
       quote_observed_at, quote_age_seconds,
       threshold_ev_percent, max_quote_age_seconds,
       detected_at
  FROM ev_signals
 WHERE league_id = @league_id
   AND quote_observed_at > @observed_after
   AND expected_value_percent >= @min_ev_percent
   AND (cardinality(@book_ids::TEXT[]) = 0 OR book_id = ANY(@book_ids::TEXT[]))
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
 ORDER BY expected_value_percent DESC, quote_observed_at DESC,
          selection_id DESC, book_id DESC
 LIMIT @row_limit;


-- name: ListLeagueEVSignalsAfterCursor :many
SELECT selection_id, market_id, market_type, league_id,
       book_id, reference_book_id, devig_method,
       offered_decimal, offered_implied, line,
       fair_probability, fair_decimal,
       expected_value, expected_value_percent,
       edge, edge_percent,
       kelly, fractional_kelly, kelly_fraction,
       quote_observed_at, quote_age_seconds,
       threshold_ev_percent, max_quote_age_seconds,
       detected_at
  FROM ev_signals
 WHERE league_id = @league_id
   AND quote_observed_at > @observed_after
   AND expected_value_percent >= @min_ev_percent
   AND (cardinality(@book_ids::TEXT[]) = 0 OR book_id = ANY(@book_ids::TEXT[]))
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
   AND (expected_value_percent, quote_observed_at, selection_id, book_id)
       < (@after_ev_percent::DOUBLE PRECISION, @after_quote_observed_at::TIMESTAMPTZ,
          @after_selection_id::TEXT, @after_book_id::TEXT)
 ORDER BY expected_value_percent DESC, quote_observed_at DESC,
          selection_id DESC, book_id DESC
 LIMIT @row_limit;


-- Every +EV signal ever recorded for one selection, newest first. The
-- after-the-fact evaluation path (CLAUDE.md phase 9: a signal must be evaluable
-- after the fact) joins this against the eventual close, one selection at a time.
--
-- Served by ev_signals_natural_key_idx, whose leading column is selection_id and
-- whose trailing DESC gives the ordering with no sort node.
--
-- name: ListEVSignalsForSelection :many
SELECT selection_id, book_id, reference_book_id, devig_method,
       offered_decimal, offered_implied, line,
       fair_probability, fair_decimal,
       expected_value, expected_value_percent,
       edge, edge_percent,
       kelly, fractional_kelly, kelly_fraction,
       quote_observed_at, quote_age_seconds,
       threshold_ev_percent, max_quote_age_seconds,
       detected_at
  FROM ev_signals
 WHERE selection_id = @selection_id
   AND quote_observed_at >= @from_inclusive
   AND quote_observed_at <  @to_exclusive
 ORDER BY quote_observed_at DESC, book_id DESC;


-- ============================================================================
-- READS -- arbitrage
-- ============================================================================

-- Live arbitrage, best return first, with the staleness discipline applied.
--
-- THREE BOUNDS, and all three are required rather than optional, because
-- decision 5 of the phase-9 brief is that a firehose of stale-price arbs is worse
-- than none:
--
--   @observed_after          only findings whose OLDEST leg is inside this
--                            window are candidates at all. Also the chunk
--                            filter equivalent -- arbitrage_signals is a plain
--                            table, so this is an index bound rather than
--                            partition pruning, but the effect on the plan is
--                            the same.
--   @max_leg_age_seconds     refuses a finding whose stalest leg is older than
--                            the reader is willing to trust. Applied ON TOP of
--                            the bound the detector already applied, exactly as
--                            @min_ev_percent is applied on top of
--                            threshold_ev_percent.
--   @max_spread_seconds      refuses a finding assembled from legs observed too
--                            far apart. The phase-4 gate found this to be the
--                            binding constraint: most cross-book "arbitrage" is
--                            one book not having moved yet, and the observed
--                            spread is what exposes it.
--
-- NO KEYSET CURSOR, deliberately. The bounds above make the live set small by
-- construction -- the phase-4 gate measured 68 findings across 1,065 records
-- before the age bound and far fewer after it -- and the set turns over every few
-- seconds, so a cursor would page through a list that no longer exists. The API
-- returns one ranked page and the client refreshes. If the set ever grows past a
-- page, the answer is a tighter bound, not a cursor.
--
-- @min_distinct_books lets a reader ask for the stronger single-book finding
-- (pass 1) or insist on genuine cross-book arbitrage (pass 2). Both are real; see
-- the column comment on distinct_books.
--
-- name: ListLiveArbitrageSignals :many
SELECT id, market_id, market_type, league_id, line,
       selection_count, implied_sum, return_fraction, distinct_books,
       observed_spread_seconds, oldest_leg_age_seconds, observed_at,
       legs_fingerprint,
       max_leg_age_seconds, max_observed_spread_seconds,
       detected_at
  FROM arbitrage_signals
 WHERE observed_at > @observed_after
   AND oldest_leg_age_seconds  <= @max_leg_age_seconds
   AND observed_spread_seconds <= @max_spread_seconds
   AND return_fraction         >= @min_return_fraction
   AND distinct_books          >= @min_distinct_books
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
 ORDER BY return_fraction DESC, observed_at DESC, id DESC
 LIMIT @row_limit;


-- The same list within one league. Served by arbitrage_signals_league_rank_idx.
--
-- name: ListLeagueLiveArbitrageSignals :many
SELECT id, market_id, market_type, league_id, line,
       selection_count, implied_sum, return_fraction, distinct_books,
       observed_spread_seconds, oldest_leg_age_seconds, observed_at,
       legs_fingerprint,
       max_leg_age_seconds, max_observed_spread_seconds,
       detected_at
  FROM arbitrage_signals
 WHERE league_id = @league_id
   AND observed_at > @observed_after
   AND oldest_leg_age_seconds  <= @max_leg_age_seconds
   AND observed_spread_seconds <= @max_spread_seconds
   AND return_fraction         >= @min_return_fraction
   AND distinct_books          >= @min_distinct_books
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
 ORDER BY return_fraction DESC, observed_at DESC, id DESC
 LIMIT @row_limit;


-- The legs of a set of findings, in one round trip.
--
-- Takes an ARRAY of signal ids rather than one, so rendering a page of N findings
-- is two queries and not N+1. Ordered by (signal_id, leg_index) so the caller can
-- group the result with a single pass and no map -- and leg_index is selection
-- DISPLAY order, so the legs come out in the order the UI renders them.
--
-- Served by arbitrage_signal_legs_signal_idx.
--
-- name: ListArbitrageSignalLegs :many
SELECT signal_id, leg_index, selection_id, role, book_id,
       decimal_odds, line, stake_fraction, observed_at, age_seconds
  FROM arbitrage_signal_legs
 WHERE signal_id = ANY(@signal_ids::UUID[])
 ORDER BY signal_id, leg_index;


-- ============================================================================
-- READS -- steam
-- ============================================================================
--
-- RANKED BY RECENCY, NOT BY MAGNITUDE. A steam alert is actionable only while the
-- follower books are still catching up -- that lag is the whole opportunity -- so
-- an hour-old larger move is worth less than a fresh smaller one. Magnitude is a
-- filter (@min_magnitude) and never the sort. See the comment on
-- steam_signals_recent_idx.
--
-- @window_end_after is REQUIRED: steam_signals is a hypertable with no retention
-- policy.
--
-- @min_participating_books is the reader's own version of the cross-book test.
-- The detector already applied min_followers, and a reader who only wants moves
-- that four books took part in can say so without the detector being
-- reconfigured.
--
-- name: ListSteamSignalsFirstPage :many
SELECT market_id, market_type, league_id, selection_id,
       window_start, window_end, window_seconds, hop_seconds,
       direction, delta_probability, magnitude_probability_points,
       velocity_probability_per_minute, devig_method,
       lead_book_id, lead_moved_at, followers, follower_count, participating_books,
       cross_book_correlation,
       threshold_velocity, threshold_magnitude, threshold_correlation,
       min_followers, max_follower_lag_seconds,
       detected_at
  FROM steam_signals
 WHERE window_end > @window_end_after
   AND magnitude_probability_points >= @min_magnitude
   AND participating_books          >= @min_participating_books
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
 ORDER BY window_end DESC, market_id DESC, selection_id DESC
 LIMIT @row_limit;


-- Continued from a cursor. The three-column comparison is a single index range on
-- steam_signals_recent_idx, and is total because (market_id, selection_id,
-- window_start, window_end) is unique and window_start is determined by window_end
-- for a fixed window length.
--
-- name: ListSteamSignalsAfterCursor :many
SELECT market_id, market_type, league_id, selection_id,
       window_start, window_end, window_seconds, hop_seconds,
       direction, delta_probability, magnitude_probability_points,
       velocity_probability_per_minute, devig_method,
       lead_book_id, lead_moved_at, followers, follower_count, participating_books,
       cross_book_correlation,
       threshold_velocity, threshold_magnitude, threshold_correlation,
       min_followers, max_follower_lag_seconds,
       detected_at
  FROM steam_signals
 WHERE window_end > @window_end_after
   AND magnitude_probability_points >= @min_magnitude
   AND participating_books          >= @min_participating_books
   AND (cardinality(@market_types::TEXT[]) = 0 OR market_type = ANY(@market_types::TEXT[]))
   AND (window_end, market_id, selection_id)
       < (@after_window_end::TIMESTAMPTZ, @after_market_id::TEXT, @after_selection_id::TEXT)
 ORDER BY window_end DESC, market_id DESC, selection_id DESC
 LIMIT @row_limit;


-- Every steam detection on one market, newest window first -- the event-detail
-- panel. Served by steam_signals_natural_key_idx, whose leading column is
-- market_id.
--
-- name: ListSteamSignalsForMarket :many
SELECT market_id, selection_id,
       window_start, window_end, window_seconds, hop_seconds,
       direction, delta_probability, magnitude_probability_points,
       velocity_probability_per_minute, devig_method,
       lead_book_id, lead_moved_at, followers, follower_count, participating_books,
       cross_book_correlation, detected_at
  FROM steam_signals
 WHERE market_id = @market_id
   AND window_end >= @from_inclusive
   AND window_end <  @to_exclusive
 ORDER BY window_end DESC, selection_id DESC;


-- ============================================================================
-- READS -- CLV
-- ============================================================================

-- One user's CLV rows, most recently graded first.
--
-- RETURNS LINE-MOVED AND VOIDED ROWS TOO, deliberately, and this is the one place
-- in the file that does. odds/clv.go says of a line-moved result: "Show it next to
-- the two lines in a user interface; never rank anyone by it." This is the user
-- interface. The row carries line_moved and voided so the client can render them
-- distinctly, and GetUserCLVAggregate below is where the exclusion is enforced.
--
-- Served by wager_leg_clv_user_idx (user_id, graded_at DESC, leg_id DESC).
--
-- name: ListUserCLVFirstPage :many
SELECT leg_id, wager_id, market_id, market_type, selection_id, league_id,
       taken_book_id, closing_book_id, devig_method,
       taken_line, closing_line, taken_at, closed_at,
       taken_fair, closing_fair, taken_price, closing_price,
       probability_clv, percent_clv, magnitude,
       beat_close, line_moved, leg_status, voided,
       graded_at
  FROM wager_leg_clv
 WHERE user_id = @user_id
   AND graded_at >= @graded_from
 ORDER BY graded_at DESC, leg_id DESC
 LIMIT @row_limit;


-- name: ListUserCLVAfterCursor :many
SELECT leg_id, wager_id, market_id, market_type, selection_id, league_id,
       taken_book_id, closing_book_id, devig_method,
       taken_line, closing_line, taken_at, closed_at,
       taken_fair, closing_fair, taken_price, closing_price,
       probability_clv, percent_clv, magnitude,
       beat_close, line_moved, leg_status, voided,
       graded_at
  FROM wager_leg_clv
 WHERE user_id = @user_id
   AND graded_at >= @graded_from
   AND (graded_at, leg_id) < (@after_graded_at::TIMESTAMPTZ, @after_leg_id::TEXT)
 ORDER BY graded_at DESC, leg_id DESC
 LIMIT @row_limit;


-- One user's CLV summary: the SQL form of odds.AggregateCLV.
--
-- THE THREE COUNTS ARE THE POINT. CLVAggregate reports VoidExcluded and
-- LineMovedExcluded "so that a leaderboard row can be audited: a user whose CLV
-- is computed from a third of their wagers is a different claim from one whose
-- CLV is computed from all of them." This query returns the same three numbers,
-- so the API can present a mean with its own provenance attached.
--
-- The means are unweighted arithmetic means over the COUNTED rows, matching
-- AggregateCLV exactly: "Unweighted by stake on purpose: CLV is a property of the
-- price, and stake-weighting would let a bettor buy leaderboard position by
-- sizing up." avg() over a FILTERed set is the same computation.
--
-- avg() returns NULL when nothing is countable, and that null is CORRECT and must
-- be propagated rather than coalesced to zero. AggregateCLV returns
-- ErrCLVNoSamples in that case "rather than reporting the mean of an empty set as
-- zero, which would put a user with three voided wagers on the leaderboard at
-- exactly par". A COALESCE(..., 0) here would reintroduce precisely that bug in
-- SQL. The API must render it as "no measurable wagers", never as 0.00%.
--
-- One honest divergence from AggregateCLV, recorded rather than hidden: that
-- function sums with Kahan-Babuska-Neumaier compensation because "naive summation
-- over a hundred thousand samples of magnitude ~1 carries a worst-case error
-- around 2e-6, which is larger than the margin that separates adjacent
-- leaderboard rows". PostgreSQL's avg(double precision) is a naive running sum.
-- At a single user's sample count (hundreds to low thousands) the difference is
-- far below display precision, so this query is fine for the per-user panel. The
-- LEADERBOARD is where adjacent rows are separated by tiny margins -- see the note
-- there.
--
-- THE TWO MEANS ARE NULLABLE, AND THE QUERY IS SHAPED TO MAKE sqlc SAY SO.
--
-- Written as FILTERed aggregates in the outer SELECT -- the obvious form -- sqlc
-- infers avg() as a non-null float64, and pgx then fails at runtime the first
-- time it scans the NULL that avg() over an empty set genuinely returns. Written
-- as scalar subqueries it does the same. The LEFT JOIN below is what makes it
-- emit pgtype.Float8, whose Valid flag is exactly the distinction that has to
-- survive: Valid=false is "no measurable wagers", which the API renders as such
-- and never as 0.00%.
--
-- The three-CTE shape is not decoration either. `counts` is an aggregate with no
-- GROUP BY, so it returns EXACTLY ONE ROW even when the user has no CLV rows at
-- all; `countable` returns one row or zero, and the LEFT JOIN turns zero into
-- NULL means. Collapsing the two into one SELECT with a GROUP BY would return
-- ZERO rows for a user with no history, which :one reports as pgx.ErrNoRows --
-- turning "this user has never had a measurable wager" into an error the caller
-- has to special-case instead of a row of honest zeros.
--
-- This is the SQL form of odds.AggregateCLV, and the correspondence is exact:
-- Samples, Counted, VoidExcluded, LineMovedExcluded, BeatCount, and the two
-- unweighted means. BeatRate is Counted/BeatCount and is left to the caller.
--
-- THE THREE EXCLUSION COUNTS ARE THE POINT. CLVAggregate reports VoidExcluded and
-- LineMovedExcluded "so that a leaderboard row can be audited: a user whose CLV
-- is computed from a third of their wagers is a different claim from one whose
-- CLV is computed from all of them."
--
-- The means are UNWEIGHTED arithmetic means over the counted rows, matching
-- AggregateCLV: "Unweighted by stake on purpose: CLV is a property of the price,
-- and stake-weighting would let a bettor buy leaderboard position by sizing up."
--
-- One honest divergence, recorded rather than hidden: AggregateCLV sums with
-- Kahan-Babuska-Neumaier compensation because "naive summation over a hundred
-- thousand samples of magnitude ~1 carries a worst-case error around 2e-6, which
-- is larger than the margin that separates adjacent leaderboard rows".
-- PostgreSQL's avg(double precision) is a naive running sum. At one user's sample
-- count -- hundreds to low thousands -- that error is far below display
-- precision, so this query is right for the per-user panel. Where it would matter
-- is the leaderboard; see the note there.
--
-- name: GetUserCLVAggregate :one
WITH sample AS (
    SELECT voided, line_moved, beat_close, probability_clv, percent_clv
      FROM wager_leg_clv
     WHERE user_id = @user_id
       AND graded_at >= @graded_from
       AND graded_at <  @graded_to
), counts AS (
    SELECT count(*)                                              AS samples,
           count(*) FILTER (WHERE NOT voided AND NOT line_moved) AS counted,
           count(*) FILTER (WHERE voided)                        AS void_excluded,
           count(*) FILTER (WHERE NOT voided AND line_moved)     AS line_moved_excluded,
           count(*) FILTER (WHERE NOT voided AND NOT line_moved
                                  AND beat_close)                AS beat_count
      FROM sample
), countable AS (
    SELECT avg(probability_clv) AS mean_probability_clv,
           avg(percent_clv)     AS mean_percent_clv
      FROM sample
     WHERE NOT voided AND NOT line_moved
    HAVING count(*) > 0
)
SELECT c.samples,
       c.counted,
       c.void_excluded,
       c.line_moved_excluded,
       c.beat_count,
       m.mean_probability_clv,
       m.mean_percent_clv
  FROM counts c
  LEFT JOIN countable m ON TRUE;


-- The same summary cut by league -- what a user is actually good at.
--
-- Ordered by counted DESC so the leagues a user has real evidence in come first,
-- and the tie-break is league_id so the ordering is total and the list is stable
-- between refreshes.
--
-- name: ListUserCLVByLeague :many
SELECT league_id,
       count(*) FILTER (WHERE NOT voided AND NOT line_moved) AS counted,
       count(*) FILTER (WHERE NOT voided AND NOT line_moved
                              AND beat_close)                AS beat_count,
       avg(probability_clv) FILTER (WHERE NOT voided AND NOT line_moved)
                                                             AS mean_probability_clv,
       avg(percent_clv)     FILTER (WHERE NOT voided AND NOT line_moved)
                                                             AS mean_percent_clv
  FROM wager_leg_clv
 WHERE user_id = @user_id
   AND graded_at >= @graded_from
   AND graded_at <  @graded_to
 GROUP BY league_id
HAVING count(*) FILTER (WHERE NOT voided AND NOT line_moved) > 0
 ORDER BY counted DESC, league_id;


-- ============================================================================
-- THE LEADERBOARD
-- ============================================================================
--
-- CLAUDE.md §6: "A public leaderboard on ROI and CLV, NOT RAW PROFIT."
--
-- -----------------------------------------------------------------------------
-- WHY NOT PROFIT, RESTATED SO NOBODY RE-ADDS IT
-- -----------------------------------------------------------------------------
-- Raw profit ranks by stake size and by variance. The user who staked the maximum
-- on one coin flip and won tops a profit board, and the ranking then teaches
-- exactly the behaviour a responsible-gaming-aware product should not. Both
-- measures below are normalised against that:
--
--   ROI = SUM(net_return) / SUM(stake), over SETTLED wagers. Stake-normalised, so
--         betting bigger cannot improve it. A losing bettor has a negative ROI at
--         any stake size, which is the gate: a HIGH-STAKE LOSING BETTOR CANNOT
--         OUTRANK A LOW-STAKE WINNING ONE, structurally, at any sample size.
--
--   CLV = the unweighted mean of percent_clv over countable rows. Unweighted for
--         AggregateCLV's stated reason: "stake-weighting would let a bettor buy
--         leaderboard position by sizing up".
--
-- -----------------------------------------------------------------------------
-- WHICH WAGERS COUNT, EXACTLY
-- -----------------------------------------------------------------------------
--   status IN ('won', 'lost', 'push', 'cashed_out')
--
-- The four TERMINAL statuses with action. 'placed' and 'open' are excluded
-- because they are unresolved -- including an open wager's stake in the
-- denominator with a null return would drag every active bettor's ROI toward -1.
--
-- 'void' IS EXCLUDED FROM BOTH NUMERATOR AND DENOMINATOR, and this is the
-- decision most likely to be re-argued. A void wager had NO ACTION: 00006's
-- wagers_return_matches_outcome pins returned_minor = stake_minor, so its net
-- return is exactly zero and including it would leave the numerator unchanged
-- while inflating the denominator -- pulling every ROI toward zero in proportion
-- to how many of a user's wagers were cancelled by the book. That is turnover
-- that never happened. It also matches odds.CLVSample.Void, which is excluded
-- "from every statistic, numerator and denominator alike", so the two halves of
-- the board apply the same rule to the same events.
--
-- A PUSH IS NOT A VOID and does count. A push had action -- the price was taken
-- and the market closed -- and only the settlement returned the stake. Excluding
-- pushes would drop the most common outcome on totals and flatter anyone who
-- specialises in them.
--
-- 'cashed_out' counts, with the return being whatever was paid at cash-out. A
-- cash-out is a real settled outcome and excluding it would let a user launder a
-- losing position out of their record.
--
-- -----------------------------------------------------------------------------
-- MONEY STAYS INTEGER
-- -----------------------------------------------------------------------------
-- staked_minor and net_return_minor are BIGINT minor units, cast back explicitly
-- because sum(bigint) returns NUMERIC in PostgreSQL -- 00006's account_balances
-- view casts for exactly this reason: "numeric would arrive in Go as a decimal
-- string and force a parse onto the balance path."
--
-- These two columns are AGGREGATES, so sqlc.yaml's note applies and they arrive
-- as bare int64 rather than domain.Money. That is correct and the reason is
-- there: every stored money column is bounded by CHECK to +/-domain.MaxSafeMoney,
-- but a SUM is not, so the read boundary must pass them through
-- domain.FromMinorUnits, which errors on overflow. Do not add an override.
--
-- roi is a RATIO and therefore a float, per CLAUDE.md §12's split. The
-- denominator cannot be zero: wagers_stake_range pins stake_minor > 0 and the
-- HAVING guarantees at least @min_settled_wagers >= 1 rows.
--
-- -----------------------------------------------------------------------------
-- THE MINIMUM-SAMPLE THRESHOLDS, AND WHY THERE ARE TWO
-- -----------------------------------------------------------------------------
-- @min_settled_wagers and @min_clv_samples are both parameters, both applied, and
-- neither defaults inside the query -- the API declares the values and can show
-- them next to the board, which is the honest presentation.
--
-- Without them a single maximum-stake winning wager is an ROI of +0.9 on a sample
-- of one and tops the board forever. That is the "one lucky bet cannot top the
-- board" requirement, and it is a THRESHOLD rather than a confidence interval on
-- purpose: a Wilson or bootstrap interval is the statistically better answer and
-- is unexplainable on a public page, where "minimum 25 settled wagers" is
-- immediately understandable and is the convention every real leaderboard uses.
--
-- Two thresholds rather than one because the two measures have different
-- denominators. A user can have fifty settled wagers and three countable CLV rows
-- (the rest line-moved), and ranking them on a three-sample CLV mean next to
-- someone with fifty is the same defect the wager threshold exists to prevent.
--
-- THE CLV JOIN IS INNER, not LEFT, and that is load-bearing rather than
-- incidental: a user with no countable CLV samples is ABSENT from the board
-- rather than present with a null or a zero. AggregateCLV makes the same choice
-- by returning ErrCLVNoSamples, "rather than reporting the mean over nothing".
--
-- Non-active users are excluded by joining `users`. NOTE THAT NO COLUMN OF
-- `users` IS SELECTED. This is a PUBLIC board and users has no display name --
-- only an email address, which must never leave the API. The board returns
-- user_id and the API maps it to whatever public handle exists; a future
-- `users.display_name` is the right place for that, not this query.
--
-- -----------------------------------------------------------------------------
-- PRECISION
-- -----------------------------------------------------------------------------
-- avg(double precision) is a naive running sum where odds.AggregateCLV uses
-- Kahan-Babuska-Neumaier compensation, and clv.go is explicit that the difference
-- matters at leaderboard scale: naive summation's worst-case error "is larger
-- than the margin that separates adjacent leaderboard rows".
--
-- Mitigated rather than ignored, in two ways. First, the sum here is per USER and
-- not over the whole population, so n is a user's sample count -- hundreds, not
-- the hundred thousand clv.go's worst case assumes. Second, the tie-break chain
-- below is deterministic and terminates in user_id, so two rows whose means are
-- equal to within float error still order stably between refreshes rather than
-- swapping places.
--
-- If a user ever accumulates enough samples for this to bite, the fix is to hand
-- the rows to odds.AggregateCLV in Go rather than to make PostgreSQL compensate.
-- ListUserCLVFirstPage already returns exactly what that function consumes.
--
-- -----------------------------------------------------------------------------
-- THE TIE-BREAK
-- -----------------------------------------------------------------------------
--   ROI board:  roi DESC, mean_percent_clv DESC, settled_wagers DESC, user_id ASC
--   CLV board:  mean_percent_clv DESC, roi DESC, clv_samples DESC, user_id ASC
--
-- Each ranks on its own measure, breaks with the OTHER measure -- so a tie on
-- results is broken by process, which is the ordering the charter's preference
-- for CLV over profit implies -- then by SAMPLE COUNT, so the better-evidenced
-- record wins a genuine tie, and finally by user_id, which is a primary key and
-- therefore makes the ordering TOTAL. Without that last component two equal rows
-- swap places between refreshes and the board looks broken.

-- name: LeaderboardByROI :many
WITH settled AS (
    SELECT w.user_id,
           count(*)                        AS settled_wagers,
           sum(w.stake_minor)::BIGINT      AS staked_minor,
           sum(w.net_return_minor)::BIGINT AS net_return_minor
      FROM wagers w
     WHERE w.status IN ('won', 'lost', 'push', 'cashed_out')
       AND w.placed_at >= @from_inclusive
       AND w.placed_at <  @to_exclusive
     GROUP BY w.user_id
), clv AS (
    SELECT c.user_id,
           count(*)                                AS clv_samples,
           count(*) FILTER (WHERE c.beat_close)    AS beat_count,
           avg(c.probability_clv)                  AS mean_probability_clv,
           avg(c.percent_clv)                      AS mean_percent_clv
      FROM wager_leg_clv c
     WHERE NOT c.voided
       AND NOT c.line_moved
       AND c.graded_at >= @from_inclusive
       AND c.graded_at <  @to_exclusive
     GROUP BY c.user_id
)
-- u.id, not s.user_id: a CTE column carries no table identity, so sqlc's
-- `users.id -> domain.UserID` override cannot reach it and the field would come
-- out a bare string. Selecting it from the joined base table is what keeps the
-- leaderboard's identifier typed. The join is an equality, so the two are the
-- same value.
--
-- Both ratios are wrapped in an explicit CAST for the same class of reason: sqlc
-- does not infer a type through a division and emits int32 for the expression,
-- which would silently truncate every ROI on the board to zero. The inner casts
-- make the arithmetic float; the outer CAST is what the code generator reads.
SELECT u.id AS user_id,
       s.settled_wagers,
       s.staked_minor,
       s.net_return_minor,
       CAST(s.net_return_minor::DOUBLE PRECISION
            / s.staked_minor::DOUBLE PRECISION AS DOUBLE PRECISION) AS roi,
       c.clv_samples,
       c.beat_count,
       CAST(c.beat_count::DOUBLE PRECISION
            / c.clv_samples::DOUBLE PRECISION AS DOUBLE PRECISION)  AS beat_rate,
       c.mean_probability_clv,
       c.mean_percent_clv
  FROM settled s
  JOIN clv   c ON c.user_id = s.user_id
  JOIN users u ON u.id      = s.user_id
 WHERE u.status = 'active'
   AND s.settled_wagers >= @min_settled_wagers::BIGINT
   AND c.clv_samples    >= @min_clv_samples::BIGINT
 ORDER BY roi DESC, mean_percent_clv DESC, settled_wagers DESC, s.user_id
 LIMIT @row_limit;


-- The same board ranked on CLV. A separate statement rather than a parameterised
-- ORDER BY: sqlc emits a prepared statement per query and a dynamic sort column
-- would have to be interpolated, which is both a plan-cache miss and the one
-- place a string ever gets concatenated into SQL in this repository.
--
-- name: LeaderboardByCLV :many
WITH settled AS (
    SELECT w.user_id,
           count(*)                        AS settled_wagers,
           sum(w.stake_minor)::BIGINT      AS staked_minor,
           sum(w.net_return_minor)::BIGINT AS net_return_minor
      FROM wagers w
     WHERE w.status IN ('won', 'lost', 'push', 'cashed_out')
       AND w.placed_at >= @from_inclusive
       AND w.placed_at <  @to_exclusive
     GROUP BY w.user_id
), clv AS (
    SELECT c.user_id,
           count(*)                                AS clv_samples,
           count(*) FILTER (WHERE c.beat_close)    AS beat_count,
           avg(c.probability_clv)                  AS mean_probability_clv,
           avg(c.percent_clv)                      AS mean_percent_clv
      FROM wager_leg_clv c
     WHERE NOT c.voided
       AND NOT c.line_moved
       AND c.graded_at >= @from_inclusive
       AND c.graded_at <  @to_exclusive
     GROUP BY c.user_id
)
-- u.id, not s.user_id: a CTE column carries no table identity, so sqlc's
-- `users.id -> domain.UserID` override cannot reach it and the field would come
-- out a bare string. Selecting it from the joined base table is what keeps the
-- leaderboard's identifier typed. The join is an equality, so the two are the
-- same value.
--
-- Both ratios are wrapped in an explicit CAST for the same class of reason: sqlc
-- does not infer a type through a division and emits int32 for the expression,
-- which would silently truncate every ROI on the board to zero. The inner casts
-- make the arithmetic float; the outer CAST is what the code generator reads.
SELECT u.id AS user_id,
       s.settled_wagers,
       s.staked_minor,
       s.net_return_minor,
       CAST(s.net_return_minor::DOUBLE PRECISION
            / s.staked_minor::DOUBLE PRECISION AS DOUBLE PRECISION) AS roi,
       c.clv_samples,
       c.beat_count,
       CAST(c.beat_count::DOUBLE PRECISION
            / c.clv_samples::DOUBLE PRECISION AS DOUBLE PRECISION)  AS beat_rate,
       c.mean_probability_clv,
       c.mean_percent_clv
  FROM settled s
  JOIN clv   c ON c.user_id = s.user_id
  JOIN users u ON u.id      = s.user_id
 WHERE u.status = 'active'
   AND s.settled_wagers >= @min_settled_wagers::BIGINT
   AND c.clv_samples    >= @min_clv_samples::BIGINT
 ORDER BY mean_percent_clv DESC, roi DESC, clv_samples DESC, s.user_id
 LIMIT @row_limit;
