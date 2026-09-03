-- =============================================================================
-- 00009  analytics  (phase 9 -- the signal store and the CLV ledger)
-- =============================================================================
--
-- CLAUDE.md §6, Analytics: "Positive-EV finder against a sharp reference book.
-- Arbitrage and middle detection across books. Steam-move alerts. CLV tracking
-- per user. [...] A public leaderboard on ROI and CLV, not raw profit."
--
-- CLAUDE.md §11, phase 9: "Analytics IN GO [...] This is the reference
-- implementation phase 12 validates against."
--
-- CLAUDE.md §3 on Flink: the phase-12 SQL jobs "replace that implementation" and
-- are checked by "same inputs, same outputs, or the Flink job is wrong".
--
-- THAT LAST SENTENCE IS WHY THIS FILE IS SHAPED THE WAY IT IS. Every table below
-- is the DURABLE RECORD OF A DERIVED FINDING, and in eighteen months a Flink SQL
-- job has to produce byte-identical rows from the same Kafka topics. So each
-- table carries not only the finding but THE PARAMETERS THAT PRODUCED IT -- the
-- threshold that was in force, the devig method that was used, the staleness
-- bound that was applied, the window that was measured. A signal row that says
-- "this was +EV" without saying "+EV by more than 2.0% measured against
-- book pinnacle with Shin devigging on a quote no older than 45 seconds" is not
-- reproducible, and a non-reproducible reference implementation cannot validate
-- anything.
--
-- Depends on:
--   00001  timescaledb extension and the authoritative ENUM CATALOGUE
--   00002  leagues, books, markets(id, type), selections -- the identity spine
--   00003  prices -- the hypertable every one of these findings is derived FROM
--   00005  users -- the leaderboard's subject
--   00006  wagers(id), legs(id) -- what a CLV row is about
--   00007  market_suspensions -- read by the closing-snapshot query, not by this
--          file's DDL; noted because the two are one design and the query's
--          correctness depends on that table's one-open-episode invariant
--
--
-- =============================================================================
-- WHAT IS APPEND-ONLY HERE, AND WHAT IS NOT. READ THIS BEFORE ADDING A TABLE.
-- =============================================================================
--
-- 00003 makes `prices` immutable by trigger and 00006 does the same to
-- `ledger_entries`. NOTHING IN THIS FILE IS APPEND-ONLY, and that is a decision
-- rather than an omission.
--
-- The distinction is OBSERVATION versus DERIVATION. A price is an observation:
-- it happened, nobody may edit it, and rewriting it invalidates every number
-- computed from it. A ledger entry is a financial fact with the same property.
-- Everything in this file is a FUNCTION of those observations plus a set of
-- parameters. Re-run the function over the same inputs with the same parameters
-- and you must get the same row back; re-run it with a corrected detector and
-- you SHOULD get a corrected row. Freezing derived rows would mean a detector
-- bug is permanent in storage, and would make the phase-12 cutover -- which is
-- precisely "recompute these rows with a different engine and diff them" --
-- impossible to perform in place.
--
-- So every table here is REPLAYABLE, and replay is made idempotent by a NATURAL
-- KEY plus an upsert rather than by a surrogate key plus an insert:
--
--   ev_signals           (selection_id, book_id, quote_observed_at)
--   steam_signals        (market_id, selection_id, window_start, window_end)
--   arbitrage_signals    (market_id, observed_at, legs_fingerprint)
--   wager_leg_clv        (leg_id)  -- the primary key; one leg has one close
--
-- Each of those is a function of the INPUT DATA ONLY. None of them contains
-- `detected_at`, and that exclusion is the whole trick: `detected_at` is a wall
-- clock reading taken at the moment the detector ran, so it differs on every
-- replay. A natural key containing it would make replay INSERT a duplicate
-- instead of updating in place, and the table would grow a new copy of every
-- finding each time a consumer group's offsets were reset. `detected_at` is
-- stored -- it is genuinely useful, it is the number that answers "how long
-- after the fact did we notice" -- but it is never part of an identity.
--
-- Phase 12 must reproduce these four keys exactly. A Flink job that keys steam
-- by (market_id, window_end) alone would collapse two sides of the same market
-- into one row and silently drop half the findings.
--
--
-- =============================================================================
-- PARTITIONING: ON EVENT TIME, NOT ON detected_at
-- =============================================================================
--
-- ev_signals and steam_signals are TimescaleDB hypertables. Neither is
-- partitioned on `detected_at`, which is the obvious choice and is wrong here for
-- two independent reasons, either of which alone would settle it:
--
--   1. A HYPERTABLE'S UNIQUE INDEX MUST CONTAIN THE PARTITIONING COLUMN. That is
--      a hard rule of the engine, not a style preference. Combine it with the
--      idempotency requirement above -- the natural key may not contain a wall
--      clock reading -- and the partitioning column is forced to be an
--      event-time column. There is no third option: partition on detected_at and
--      you cannot have a replay-stable unique key, which means you cannot upsert,
--      which means replay duplicates.
--
--   2. IT IS THE COLUMN PHASE 12 WATERMARKS FROM. 00003 already establishes
--      `prices.observed_at` as "the event-time attribute phase 12's Flink
--      watermarks will be assigned from". A Flink SQL job consuming
--      odds.normalized assigns watermarks from the provider observation instant
--      and emits results stamped in that same time domain. If this schema
--      partitioned on our own clock instead, the Go implementation and the Flink
--      implementation would be time-indexed differently and the "same inputs,
--      same outputs" diff would have to translate between two time domains
--      before it could compare anything.
--
-- So:
--     ev_signals      partitioned on quote_observed_at  (the quote's own instant)
--     steam_signals   partitioned on window_end         (the hopping window's
--                                                        closing edge, an
--                                                        event-time boundary)
--
-- Both are provider observation instants propagated unchanged, exactly as 00003
-- requires of `prices.observed_at`. Neither is ever stamped from a clock of ours.
--
--
-- WHY arbitrage_signals AND wager_leg_clv ARE PLAIN TABLES
-- ---------------------------------------------------------------------------
-- Not for symmetry, and not because the volume is low -- although it is. In both
-- cases a hypertable is mechanically ruled out by a foreign key:
--
--   arbitrage_signals   has a CHILD TABLE, arbitrage_signal_legs, which must
--                       reference it. TimescaleDB does not support a foreign key
--                       whose TARGET is a hypertable. The alternatives were to
--                       inline the legs as JSONB (which steam_signals does for
--                       its follower set, see below) or to keep the parent a
--                       plain table. The legs are a fixed-arity relational fact
--                       -- one row per outcome, each with a book, a price, a line
--                       and a stake fraction -- that the API filters and sorts
--                       by, so they are a table. The parent follows.
--
--   wager_leg_clv       references legs(id) and wagers(id), which is the
--                       direction Timescale DOES allow, but its own primary key
--                       is leg_id and a hypertable's primary key must contain the
--                       partitioning column. Adding a time column to that key
--                       would break the exact property that makes this table
--                       idempotent for free: one leg has exactly one close.
--
-- The volume argument agrees rather than leads. The phase-4 gate measured 68 live
-- arbitrage findings over 1,065 records with the leg-age bound binding
-- constantly, and a CLV row exists only per GRADED LEG, which is human-rate.
-- Neither table will see the 4x10^5..2.7x10^6 rows/day that made `prices` a
-- hypertable. A plain table with a btree index on its time column is the right
-- shape at this size, and `make query-plans` will say so.
--
--
-- STEAM FOLLOWERS ARE JSONB, AND THAT IS A CONTRACT DECISION
-- ---------------------------------------------------------------------------
-- `steam_signals.followers` holds the books that followed the lead book and the
-- lag each took, as a JSON array. It is not a child table, for the FK reason
-- above, and given a choice it is also the shape phase 12 wants: a Flink SQL job
-- emits one row per detection carrying an ARRAY<ROW<...>> of followers, which
-- serialises to exactly this JSON and would have to be shredded into a second
-- sink to become a child table. The array's shape is constrained by CHECK rather
-- than trusted -- see the constraint block on the table.
--
--
-- =============================================================================
-- THE MONEY / PROBABILITY SPLIT, RESTATED BECAUSE THIS FILE IS FULL OF NUMBERS
-- =============================================================================
--
-- CLAUDE.md §12: "All money and stake values are integer minor units. Floating
-- point never touches a balance. Odds and probabilities are floats; ledger
-- amounts are not."
--
-- There is exactly ONE money column in this entire file -- and it is not stored,
-- it is summed by the leaderboard query out of `wagers`. Every column declared
-- below is an odds, a probability, a ratio, a rate, a duration or a count. That
-- is not an accident of the feature set: a SIGNAL IS A STATEMENT ABOUT A PRICE,
-- and stake sizing (Kelly) is expressed as a FRACTION OF BANKROLL precisely so
-- that no signal has to know a balance. Storing "stake $37.42" on an EV signal
-- would bind a finding about a market to one user's bankroll.
--
-- Every probability column is bounded (0, 1) EXCLUSIVE, matching
-- odds.Probability's own useful range: a fair probability of exactly 0 or 1 has
-- no finite decimal price and divides by zero in the EV, Kelly and CLV formulas.
-- Every decimal-odds column repeats 00003's bounds character for character --
-- `> 1.0 AND <= 100000.0`, which is domain.MinDecimalOdds exclusive and
-- MaxDecimalOdds inclusive, and which also refuses NaN and both infinities for
-- the reason 00003 spells out (PostgreSQL orders NaN GREATER than every float,
-- so it passes `> 1.0` and fails `<= 100000.0`).
--
-- Unbounded floats -- velocities, CLV deltas, ages -- get an explicit finiteness
-- test written as an ordering comparison rather than as `x = x`, because
-- PostgreSQL defines NaN as equal to itself. Same spelling as 00002's
-- markets_line_finite.
--
--
-- =============================================================================
-- WHAT IS DELIBERATELY NOT HERE
-- =============================================================================
--
-- MIDDLES. `internal/pricing` already emits MiddleRef on ComputedMarket and
-- phase 4 tested it, but CLAUDE.md §6's Analytics bullet names "Arbitrage and
-- middle detection" as ONE capability and §11 phase 9 does not ask for a middle
-- ALERT. A middle is a position, not an event: it exists for as long as the two
-- lines disagree and it is evaluated by hit probability rather than by a
-- guaranteed return, so the arbitrage table's "return, distinct books, oldest
-- leg age" shape does not fit it. Adding `middle_signals` later is a new
-- migration with no change to anything here. Recorded so the absence reads as a
-- decision rather than as a gap.
--
-- SIGNAL ACKNOWLEDGEMENT / ALERT DELIVERY STATE. Whether a user has seen a
-- steam alert is user state, not a property of the finding, and putting it here
-- would make a derived table non-recomputable -- a replay would wipe it. It
-- belongs in a table owned by whatever ships alerts.
--
-- A `signals` SUPERTYPE. Four tables with a shared header and a discriminator
-- would look tidier and would be worse: the four findings share almost no
-- columns (an EV signal has one book, an arb has several; steam has a window,
-- CLV has two instants), so the supertype would be four nullable column groups
-- and a CHECK matrix, and every query would carry a discriminator predicate the
-- planner has to filter on. Phase 12 also emits them to four separate Kafka
-- topics, so there is no join that wants them in one relation.
-- =============================================================================

-- +goose Up

-- -----------------------------------------------------------------------------
-- The updated_at trigger function.
--
-- Namespaced `analytics_` for the reason 00002, 00005, 00006 and 00007 each give
-- for their own copy: a bare `set_updated_at()` is the name every concurrently
-- authored migration reaches for, and the first Down to run would drop a
-- function another migration's triggers still depend on. Five near-identical
-- copies is the price of each migration's Down having a blast radius of exactly
-- itself, and 00001 records that trade rather than resolving it.
--
-- Every table in this file is mutable -- see the append-only discussion in the
-- header -- so every table in this file gets one.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION analytics_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $analytics_set_updated_at$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$analytics_set_updated_at$;
-- +goose StatementEnd

COMMENT ON FUNCTION analytics_set_updated_at() IS
    'Stamps updated_at on every UPDATE against a phase-9 analytics table. Namespaced to migration 00009 so this file''s Down cannot drop a function another migration''s triggers depend on.';


-- =============================================================================
-- ev_signals
-- =============================================================================
--
-- One positive-expected-value finding: a quote at one book on one selection that
-- beat the fair probability derived from the sharp reference book by more than
-- the threshold in force.
--
-- CLAUDE.md §11 phase 9 asks for a "+EV finder". The persistence requirement is
-- the part that shapes the table: a finding is only worth anything if it can be
-- EVALUATED AFTER THE FACT. "Our +EV finder fired 4,000 times last month" is
-- worthless; "our +EV finder fired 4,000 times and those selections beat the
-- close 61% of the time" is the entire claim the feature makes. That second
-- sentence is a join between this table and `wager_leg_clv`'s closing snapshot --
-- or, for signals nobody bet, between this table and a closing snapshot computed
-- on demand. Both joins need the same three things off this row:
--
--     selection_id + market_id     what to look up the close for
--     quote_observed_at            when the finding was true
--     fair_probability             what we claimed at the time
--
-- and all three are here, NOT NULL.
--
-- NOTHING IS COMPUTED FROM ANOTHER COLUMN AT READ TIME. expected_value_percent,
-- edge_percent, kelly and fractional_kelly are all derivable from
-- (fair_probability, offered_decimal, kelly_fraction) with the formulas in
-- internal/domain/odds. They are stored anyway, because the point of the table is
-- to record WHAT THE DETECTOR SAID, not what a later reader can re-derive: if the
-- EV formula is ever corrected, a stored row still shows the number that was
-- shown to a user, and the diff between stored and re-derived is exactly the
-- audit phase 12 performs.
--
-- FOR THE SAME REASON THERE ARE NO ARITHMETIC-IDENTITY CHECK CONSTRAINTS on
-- those columns, which is a departure from 00006's style (wagers_net_return_
-- identity, wagers_potential_profit_identity). Those are integer identities and
-- exact. These are float64 expressions evaluated in Go through
-- odds.ExpectedValue / odds.Kelly, and a re-derivation in SQL is not obliged to
-- produce a bit-identical double -- the association order differs, and Postgres
-- is free to evaluate `p * d - 1` differently from Go. A CHECK that is right
-- 99.99% of the time and rejects a correct row on the other 0.01% is worse than
-- no CHECK. The identities are asserted where they can be asserted exactly:
-- internal/domain/odds's property tests.
--
-- What IS checked is the thing that makes the row a +EV signal at all: EV, edge
-- and Kelly are all strictly positive. A row failing that is not a
-- floating-point edge case, it is a detector that emitted a finding it should
-- have filtered.
-- -----------------------------------------------------------------------------
CREATE TABLE ev_signals (
    -- The selection the finding is about. FK ON DELETE RESTRICT, continuing
    -- 00002's refusal to cascade down the identity spine; the charset CHECK is
    -- domain.validID, spelled identically to 00002's selections_id_charset.
    selection_id           TEXT             NOT NULL
                                            REFERENCES selections (id) ON DELETE RESTRICT
                                            CONSTRAINT ev_signals_selection_id_charset
                                            CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- market_id and market_type are DENORMALISED from selections, and pinned
    -- together by a composite FK to markets (id, type) exactly as 00006's `legs`
    -- does. Two reasons, and the second is the load-bearing one:
    --
    --   * The API filters +EV signals by market type (§6: "filtered by threshold,
    --     league, book and market type"). Without these columns that filter is a
    --     join through selections to markets on every page of a ranked list.
    --   * The composite FK makes the copy UNFORGEABLE. A row claiming
    --     ('mkt-123', 'total') when market mkt-123 is a moneyline is rejected by
    --     the database, so the denormalisation cannot drift from the catalogue.
    --     A plain `market_id TEXT` plus a plain `market_type TEXT` could.
    market_id              TEXT             NOT NULL
                                            CONSTRAINT ev_signals_market_id_charset
                                            CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    market_type            TEXT             NOT NULL
                                            CONSTRAINT ev_signals_market_type_defined
                                            CHECK (market_type IN ('moneyline', 'spread', 'total',
                                                                   'player_prop', 'futures')),

    -- Denormalised two levels further, from markets -> events -> leagues, and it
    -- is the one denormalisation here with no composite FK to pin it, because
    -- there is no (market_id, league_id) unique pair to point at. The alternative
    -- is a three-table join on the hottest analytics read in the product, and
    -- CLAUDE.md §6 lists league filtering as a first-class control on the +EV
    -- finder. The writer derives it from the same ComputedMarket that carries the
    -- market and event refs, so the two cannot disagree at the source.
    league_id              TEXT             NOT NULL
                                            REFERENCES leagues (id) ON DELETE RESTRICT
                                            CONSTRAINT ev_signals_league_id_charset
                                            CHECK (league_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- The book OFFERING the price. This is the book a user would bet at.
    book_id                TEXT             NOT NULL
                                            REFERENCES books (id) ON DELETE RESTRICT
                                            CONSTRAINT ev_signals_book_id_charset
                                            CHECK (book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- The book the FAIR VALUE was derived from (ADR 0006: fair value comes from
    -- the sharp reference book, not from a consensus of everyone). Recorded per
    -- row rather than assumed from config, because the reference book is
    -- selectable and a month-old signal must still say what it was measured
    -- against. A signal measured against a different reference is a different
    -- claim, and the two must not be silently compared.
    --
    -- reference_book_id = book_id is legal and is not a bug: it means the sharp
    -- book's own quote was +EV against its own devigged fair value, which happens
    -- on an under-round market and which odds/vig.go names as a real, findable
    -- condition.
    reference_book_id      TEXT             NOT NULL
                                            REFERENCES books (id) ON DELETE RESTRICT
                                            CONSTRAINT ev_signals_reference_book_id_charset
                                            CHECK (reference_book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Which of odds.DevigMethod's four produced fair_probability. The four
    -- disagree meaningfully on longshots -- that is why CLAUDE.md §4 requires all
    -- four to exist -- so a fair probability without its method is not a
    -- reproducible number. Spelled with DevigMethod.String()'s canonical
    -- lowercase names; odds.ParseDevigMethod is the read boundary.
    devig_method           TEXT             NOT NULL
                                            CONSTRAINT ev_signals_devig_method_defined
                                            CHECK (devig_method IN ('multiplicative', 'additive',
                                                                    'power', 'shin')),

    -- The quote itself. Bounds are 00003's prices_decimal_odds_range verbatim.
    offered_decimal        DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_offered_decimal_range
                                            CHECK (offered_decimal > 1.0
                                                   AND offered_decimal <= 100000.0),

    -- 1/offered_decimal, the vigged implied probability. Stored rather than
    -- derived for the same reason as the rest: it is what the detector saw.
    offered_implied        DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_offered_implied_range
                                            CHECK (offered_implied > 0.0
                                                   AND offered_implied < 1.0),

    -- The line the QUOTE was made at, from the selection's own perspective --
    -- domain.Price.Line semantics, already inverted for an away spread, exactly
    -- as 00003's prices.line. NULL is domain.NoLine(); 0.0 is a stored pick'em.
    -- Finiteness spelled as 00002's markets_line_finite.
    line                   DOUBLE PRECISION
                                            CONSTRAINT ev_signals_line_finite
                                            CHECK (line IS NULL
                                                   OR (line > '-Infinity'::double precision
                                                       AND line < 'Infinity'::double precision)),

    -- The devigged fair probability and its decimal form 1/p, from the reference
    -- book's full outcome set.
    fair_probability       DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_fair_probability_range
                                            CHECK (fair_probability > 0.0
                                                   AND fair_probability < 1.0),
    fair_decimal           DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_fair_decimal_range
                                            CHECK (fair_decimal > 1.0
                                                   AND fair_decimal <= 100000.0),

    -- pricing.QuoteAssessment's ExpectedValue (per unit staked) and
    -- ExpectedValuePercent (the same number x100). Both strictly positive: see
    -- the header note -- this is the constraint that makes the row a +EV signal
    -- rather than an assessment.
    expected_value         DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_expected_value_positive
                                            CHECK (expected_value > 0.0
                                                   AND expected_value < 'Infinity'::double precision),
    expected_value_percent DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_expected_value_percent_positive
                                            CHECK (expected_value_percent > 0.0
                                                   AND expected_value_percent < 'Infinity'::double precision),

    -- Edge = fair_probability - offered_implied, in probability points, and the
    -- same as a percentage. A distinct quantity from EV and not a rescaling of
    -- it: EV is per unit STAKED, edge is per unit of PROBABILITY, and the two
    -- rank a slate differently at long prices.
    edge                   DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_edge_positive
                                            CHECK (edge > 0.0 AND edge < 1.0),
    edge_percent           DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_edge_percent_positive
                                            CHECK (edge_percent > 0.0 AND edge_percent < 100.0),

    -- Full Kelly and fractional Kelly, both as a FRACTION OF BANKROLL in (0, 1].
    -- No money column: see the header. A staking amount is the product of this
    -- and a bankroll the signal deliberately does not know.
    kelly                  DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_kelly_range
                                            CHECK (kelly > 0.0 AND kelly <= 1.0),
    fractional_kelly       DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_fractional_kelly_range
                                            CHECK (fractional_kelly > 0.0 AND fractional_kelly <= 1.0),

    -- The multiplier applied to full Kelly, so fractional_kelly is reproducible.
    -- Without it a stored 0.0125 is unattributable: it could be quarter Kelly on
    -- a 5% edge or half Kelly on a 2.5% one.
    kelly_fraction         DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_kelly_fraction_range
                                            CHECK (kelly_fraction > 0.0 AND kelly_fraction <= 1.0),
    CONSTRAINT ev_signals_fractional_kelly_not_larger
        CHECK (fractional_kelly <= kelly),

    -- THE PARTITIONING COLUMN. The provider's own observation instant for the
    -- quote, propagated unchanged from prices.observed_at -- which is to say
    -- unchanged all the way from the provider payload, per the SLO definition
    -- 00003's header quotes from the Grafana dashboard. Never stamped from a
    -- clock of ours. Lower bound spelled as 00003's prices_observed_at_sane.
    quote_observed_at      TIMESTAMPTZ      NOT NULL
                                            CONSTRAINT ev_signals_quote_observed_at_sane
                                            CHECK (quote_observed_at > '1900-01-01T00:00:00Z'),

    -- The quote's age at the moment the detector ran, in seconds. May be
    -- NEGATIVE, and the CHECK deliberately permits that: domain.Price.Age()
    -- returns a negative duration for a future-stamped observation and price.go
    -- explains why -- "returning it rather than clamping to zero is what lets a
    -- monitor detect the skew instead of silently reporting healthy staleness".
    -- 00003 declines a CHECK (ingested_at >= observed_at) on the same grounds.
    -- Finiteness is still required.
    quote_age_seconds      DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_quote_age_finite
                                            CHECK (quote_age_seconds > '-Infinity'::double precision
                                                   AND quote_age_seconds < 'Infinity'::double precision),

    -- ------------------------------------------------------------------------
    -- THE THRESHOLDS THAT WERE IN FORCE. These two columns are what make the
    -- table auditable, and they are not decoration.
    --
    -- A signal store without them answers "was this +EV?" and cannot answer "why
    -- did the count triple in March?" -- which is almost always because somebody
    -- lowered a threshold, and with the threshold unrecorded there is no way to
    -- separate that from the market changing. They also make the phase-12 diff
    -- meaningful: a Flink job configured with a different threshold produces a
    -- different row SET, and comparing the sets without comparing the thresholds
    -- would report the Flink job as wrong when it is merely differently
    -- configured.
    --
    -- threshold_ev_percent is a documented, declared bound rather than a magic
    -- number buried in the detector; max_quote_age_seconds is the staleness bound
    -- of decision 5's arbitrage discipline applied to the EV finder, because a
    -- +EV finding against a quote nobody could still take is the same defect in a
    -- different costume.
    -- ------------------------------------------------------------------------
    threshold_ev_percent   DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_threshold_ev_percent_range
                                            CHECK (threshold_ev_percent >= 0.0
                                                   AND threshold_ev_percent < 'Infinity'::double precision),
    max_quote_age_seconds  DOUBLE PRECISION NOT NULL
                                            CONSTRAINT ev_signals_max_quote_age_positive
                                            CHECK (max_quote_age_seconds > 0.0
                                                   AND max_quote_age_seconds < 'Infinity'::double precision),
    CONSTRAINT ev_signals_meets_own_threshold
        CHECK (expected_value_percent >= threshold_ev_percent),

    -- When the detector emitted this. OUR clock, not the provider's, and
    -- therefore never part of the natural key -- see the header. Kept because
    -- (detected_at - quote_observed_at) is the analytics-stage equivalent of the
    -- staleness SLO: how long after a price moved did we notice it was +EV.
    detected_at            TIMESTAMPTZ      NOT NULL
                                            CONSTRAINT ev_signals_detected_at_sane
                                            CHECK (detected_at > '1900-01-01T00:00:00Z'),

    created_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ      NOT NULL DEFAULT now(),

    -- Pins the denormalised market_type to the catalogue. ON UPDATE RESTRICT
    -- rather than 00002's CASCADE: a market that changed type is a different
    -- market, and a signal about the old one must not silently re-point.
    CONSTRAINT ev_signals_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE RESTRICT,

    -- The line rule 00002 enforces on `markets` and 00006 repeats on `legs`,
    -- repeated here for the same reason: a moneyline signal carrying a line means
    -- the writer invented one.
    CONSTRAINT ev_signals_line_rule
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN line IS NULL
                   WHEN 'futures'     THEN line IS NULL
                   WHEN 'spread'      THEN line IS NOT NULL
                   WHEN 'total'       THEN line IS NOT NULL AND line > 0
                   WHEN 'player_prop' THEN TRUE
                   ELSE FALSE
               END)
);

COMMENT ON TABLE ev_signals IS
    'CLAUDE.md §6 Analytics: one positive-EV finding against the sharp reference book. Hypertable partitioned on quote_observed_at (the PROVIDER''s instant), NOT on detected_at -- see migration 00009''s header for why event-time partitioning is forced by the idempotent-replay requirement. Derived and recomputable: upsert on (selection_id, book_id, quote_observed_at).';
COMMENT ON COLUMN ev_signals.book_id IS
    'The book OFFERING the price -- where a user would place the bet.';
COMMENT ON COLUMN ev_signals.reference_book_id IS
    'The sharp book the fair value was devigged from (ADR 0006). Stored per row because the reference is selectable: a signal measured against a different reference is a different claim and the two must not be compared.';
COMMENT ON COLUMN ev_signals.devig_method IS
    'Which of odds.DevigMethod''s four produced fair_probability. The four disagree meaningfully on longshots, so a fair probability without its method is not reproducible.';
COMMENT ON COLUMN ev_signals.quote_observed_at IS
    'Partitioning column. The provider''s own instant for the quote, propagated unchanged from prices.observed_at. Part of the natural key, which is why it and not detected_at is the time dimension.';
COMMENT ON COLUMN ev_signals.quote_age_seconds IS
    'The quote''s age when the detector ran. MAY BE NEGATIVE on provider clock skew, deliberately -- domain.Price.Age() returns the negative rather than clamping, so a monitor can see the skew.';
COMMENT ON COLUMN ev_signals.threshold_ev_percent IS
    'The minimum EV% in force when this fired. Recorded so a change in signal volume can be attributed to a threshold change rather than to the market, and so phase 12''s Flink job can be diffed against a like-configured run.';
COMMENT ON COLUMN ev_signals.max_quote_age_seconds IS
    'The staleness bound in force. A +EV finding on a quote nobody could still take is the same defect as a stale arbitrage; the bound is declared rather than magic.';
COMMENT ON COLUMN ev_signals.detected_at IS
    'When the pricer emitted this, from OUR clock. Never part of the natural key -- it differs on every replay, which would turn an idempotent upsert into a duplicate insert.';

-- 1-DAY CHUNKS. Sized by the same method as 00003 and landing three orders of
-- magnitude smaller.
--
-- Row rate: this table is a THRESHOLDED FILTER over `prices`, whose ceiling
-- 00003 computes at ~2.7x10^6 rows/day. Every row here requires a quote that
-- beats the reference book's devigged fair value by threshold_ev_percent AND is
-- fresher than max_quote_age_seconds. At the phase-4 gate's measured rates that
-- is low single-digit percent of quotes, so the design envelope is ~10^4
-- rows/day with an order of magnitude of headroom -- call it 10^5. At roughly
-- 320 bytes/row uncompressed (fifteen 8-byte floats plus six provider-derived
-- TEXT identifiers) a 1-day chunk at the ceiling is ~32 MB, comfortably inside
-- the 512 MB shared_buffers that 00003's arithmetic is built on, with 365
-- chunks/year.
--
-- Why not 12 hours, matching `prices`: this table is read by RANKED, TIME-BOUNDED
-- queries ("the best +EV signals in the last hour", "the last day's findings for
-- this league"), and each such query touches every chunk its window overlaps. A
-- day-long window over 12-hour chunks is 2-3 chunks; over 1-day chunks it is 1-2.
-- The chunk can afford to be coarser here precisely because the row rate is
-- ~27x lower, and 00003's rule -- "chunks as large as memory allows, so that
-- queries touch as few as possible" -- then points at the larger interval.
--
-- Why not 7 days: a 7-day chunk at the ceiling is ~224 MB write-active, and two
-- of those around a boundary is 448 MB against 512 MB of shared_buffers. That is
-- the thrash 00003 rejects 24-hour `prices` chunks for, and there is no read
-- pattern asking for it.
--
-- create_default_indexes => FALSE for 00003's and 00007's reason: the default
-- index on (quote_observed_at DESC) would be a permanent write tax serving a
-- read pattern nothing asks for. Every query below leads with a ranking column or
-- with the natural key, and time-range narrowing is what the partitioning is for.
SELECT create_hypertable(
    'ev_signals',
    by_range('quote_observed_at', INTERVAL '1 day'),
    create_default_indexes => FALSE
);

-- THE NATURAL KEY. One quote -- one (selection, book, instant) -- yields at most
-- one +EV finding, so this is both the uniqueness constraint and the arbiter for
-- the replay-idempotent upsert in queries/analytics.sql.
--
-- Contains the partitioning column as the engine requires, and contains ONLY
-- input-derived values as idempotency requires. Trailing DESC for 00003's reason:
-- "the newest signals for this selection at this book" walks backwards from the
-- boundary with no sort node, and PRIMARY KEY syntax cannot express DESC.
CREATE UNIQUE INDEX ev_signals_natural_key_idx
    ON ev_signals (selection_id, book_id, quote_observed_at DESC);

COMMENT ON INDEX ev_signals_natural_key_idx IS
    'The replay key: one quote yields at most one +EV finding. Also the ON CONFLICT arbiter that makes recomputation idempotent, and the index behind per-selection lookups.';

-- THE RANKING INDEXES, and why the column directions are all DESC.
--
-- The product read is "the best +EV signals right now", which is
--     ORDER BY expected_value_percent DESC, quote_observed_at DESC, ...
-- paged by KEYSET, because 00009's list is written continuously by the pricer and
-- OFFSET on a continuously-written set silently skips and duplicates rows --
-- api.sql's header has the full argument.
--
-- A keyset predicate is a ROW-VALUE comparison, `(a, b, c, d) < (@a, @b, @c, @d)`,
-- and PostgreSQL only plans that as a single index range when EVERY component
-- sorts the same way. A mixed ordering -- EV descending, selection_id ascending --
-- cannot be written as a row comparison at all, and expanding it into
-- `a < x OR (a = x AND ...)` is exactly where off-by-one duplicate-row bugs live.
--
-- So the ORDER BY is ALL DESCENDING, including the two identifier tie-breakers,
-- and these indexes match it. The tie-breakers are not meaningful in themselves;
-- they exist to make the ordering TOTAL so a cursor is unambiguous, and
-- descending order serves that as well as ascending would.
--
-- (expected_value_percent, quote_observed_at, selection_id, book_id) is total
-- because (selection_id, book_id, quote_observed_at) is already unique.
--
-- TWO indexes rather than one, for api.sql's stated reason: a cross-league query
-- cannot use an index led by league_id, and a league-filtered query that scanned
-- the cross-league index would have to filter most rows away before reaching the
-- page size. book_id and market_type are left as residual predicates -- both are
-- low cardinality, and after the league and time narrowing the candidate set is
-- small enough that a third and fourth index would cost more on write than they
-- save on read.
CREATE INDEX ev_signals_rank_idx
    ON ev_signals (expected_value_percent DESC, quote_observed_at DESC,
                   selection_id DESC, book_id DESC);

CREATE INDEX ev_signals_league_rank_idx
    ON ev_signals (league_id, expected_value_percent DESC, quote_observed_at DESC,
                   selection_id DESC, book_id DESC);

COMMENT ON INDEX ev_signals_rank_idx IS
    'The cross-league ranked board. All four columns DESC so the keyset cursor can be a single row-value comparison -- a mixed-direction ORDER BY cannot be expressed as one and expands into the form where off-by-one paging bugs live.';
COMMENT ON INDEX ev_signals_league_rank_idx IS
    'The same ranking within one league. A cross-league query cannot use an index led by league_id, which is why this is a second index rather than a nullable filter on the first.';

CREATE TRIGGER ev_signals_set_updated_at
    BEFORE UPDATE ON ev_signals
    FOR EACH ROW EXECUTE FUNCTION analytics_set_updated_at();


-- =============================================================================
-- arbitrage_signals  +  arbitrage_signal_legs
-- =============================================================================
--
-- One arbitrage finding: a set of quotes, one per outcome of a market, all at the
-- same line, whose implied probabilities sum below 1. The wire shape is
-- pricing.ArbitrageRef and this pair of tables is its durable form.
--
-- ---------------------------------------------------------------------------
-- THE STALENESS DISCIPLINE IS THE POINT OF THIS TABLE, NOT A FILTER ON IT
-- ---------------------------------------------------------------------------
-- The phase-4 gate found 68 live arbitrage opportunities across 1,065 records
-- with the leg-age bound binding constantly. That number is the honest
-- description of cross-book "arbitrage": MOST OF IT IS ONE BOOK NOT HAVING MOVED
-- YET. A finding assembled from a fresh quote at book A and a ninety-second-old
-- quote at book B is not an opportunity, it is a measurement of book B's polling
-- lag, and by the time a user clicks it is gone.
--
-- A firehose of stale-price arbs is worse than showing none at all, because it
-- teaches a user that the feature lies. So three columns exist purely to let a
-- reader judge credibility WITHOUT trusting that we applied a threshold:
--
--     observed_spread_seconds   the gap between the oldest and newest leg
--     oldest_leg_age_seconds    the age of the stalest leg -- an opportunity is
--                               exactly as fresh as its stalest leg
--     distinct_books            1 means a single book's own market is
--                               under-round, which is a genuinely different and
--                               much more credible finding than a cross-book one
--
-- and two more record the bounds that WERE applied:
--
--     max_leg_age_seconds
--     max_observed_spread_seconds
--
-- ArbitrageRef's own doc comment states the principle: these are "on the record
-- because it is the number that separates a credible finding from a stale one,
-- and a consumer must be able to judge that for itself rather than trust that a
-- threshold was applied."
--
-- ---------------------------------------------------------------------------
-- WHY THE MARGIN IS ONE COLUMN AND NOT FIVE
-- ---------------------------------------------------------------------------
-- pricing.Margin carries five fields: Selections, ImpliedSum, BookingPercentage,
-- Overround and Vig. Four of them are exact functions of the other two:
--
--     BookingPercentage = 100 * ImpliedSum
--     Overround         = ImpliedSum - 1
--     Vig               = (ImpliedSum - 1) / ImpliedSum
--
-- and unlike the EV columns above these ARE exact, single-operation float
-- expressions with no association-order freedom, so re-deriving them in SQL or in
-- Go gives bit-identical results. Storing all five would be four columns that can
-- only ever disagree with the first through a writer bug. implied_sum and
-- selection_count are stored; the rest is a view-level expression.
--
-- The same argument does NOT apply to ev_signals' EV/edge/Kelly columns, and the
-- difference is worth naming: those go through multi-step formulas in
-- internal/domain/odds where a SQL re-derivation is not obliged to match bit for
-- bit, AND they are the numbers a user was shown. Here the five margin fields are
-- one number in five costumes.
--
-- ---------------------------------------------------------------------------
-- PLAIN TABLE, NOT A HYPERTABLE
-- ---------------------------------------------------------------------------
-- Decided by the child table: TimescaleDB does not support a foreign key whose
-- TARGET is a hypertable, so `arbitrage_signal_legs` could not reference this if
-- it were one. The full argument, including the JSONB alternative that
-- steam_signals takes, is in this migration's header.
-- ---------------------------------------------------------------------------
CREATE TABLE arbitrage_signals (
    -- A SURROGATE key, which is a departure from this schema's TEXT-domain-id
    -- convention and is required rather than preferred: the legs table needs a
    -- single-column FK target, and the natural key below is three columns wide
    -- including a float-free but still wide (market, instant, fingerprint)
    -- tuple. Copying that into every leg row would be larger than the leg.
    --
    -- UUID rather than TEXT because nothing outside the database names an
    -- arbitrage finding -- there is no provider identifier for it, so there is no
    -- domain id to carry. 00007's market_suspensions makes the same choice for
    -- the same reason.
    id                          UUID             NOT NULL DEFAULT gen_random_uuid(),

    market_id                   TEXT             NOT NULL
                                                 CONSTRAINT arbitrage_signals_market_id_charset
                                                 CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    market_type                 TEXT             NOT NULL
                                                 CONSTRAINT arbitrage_signals_market_type_defined
                                                 CHECK (market_type IN ('moneyline', 'spread', 'total',
                                                                        'player_prop', 'futures')),
    league_id                   TEXT             NOT NULL
                                                 REFERENCES leagues (id) ON DELETE RESTRICT
                                                 CONSTRAINT arbitrage_signals_league_id_charset
                                                 CHECK (league_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- ArbitrageRef.Line: the line EVERY leg was quoted at, in the market's HOME
    -- frame. Note the frame difference from ev_signals.line and prices.line,
    -- which are stated from the selection's own perspective: an arbitrage is a
    -- statement about the market as a whole and its legs are on opposite sides,
    -- so there is no single selection whose perspective it could take. The legs
    -- table stores each leg's own line in the leg's own frame.
    line                        DOUBLE PRECISION
                                                 CONSTRAINT arbitrage_signals_line_finite
                                                 CHECK (line IS NULL
                                                        OR (line > '-Infinity'::double precision
                                                            AND line < 'Infinity'::double precision)),

    -- n, the number of outcomes the finding covers. At least 2: a one-outcome
    -- "arbitrage" is not one.
    selection_count             INTEGER          NOT NULL
                                                 CONSTRAINT arbitrage_signals_selection_count_range
                                                 CHECK (selection_count >= 2 AND selection_count <= 64),

    -- S = SUM 1/d over the legs. UNDER-ROUND BY CONSTRUCTION -- that is what makes
    -- this a finding rather than an assessment -- so the constraint is not a
    -- sanity bound, it is the definition of the table.
    implied_sum                 DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_implied_sum_underround
                                                 CHECK (implied_sum > 0.0 AND implied_sum < 1.0),

    -- ArbitrageRef.Return: guaranteed profit per unit of TOTAL OUTLAY, (1-S)/S.
    -- Strictly positive, which follows from implied_sum < 1 and is checked
    -- independently so a writer that computed it some other way is caught.
    return_fraction             DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_return_positive
                                                 CHECK (return_fraction > 0.0
                                                        AND return_fraction < 'Infinity'::double precision),

    -- How many DIFFERENT books the legs span. ONE IS LEGAL and is not a
    -- degenerate case: it means a single book's own market is under-round, which
    -- odds/vig.go names as a real and findable condition and which is far more
    -- actionable than a cross-book finding because there is no execution race.
    -- Never greater than selection_count -- one book per leg at most.
    distinct_books              INTEGER          NOT NULL
                                                 CONSTRAINT arbitrage_signals_distinct_books_range
                                                 CHECK (distinct_books >= 1),
    CONSTRAINT arbitrage_signals_books_within_legs
        CHECK (distinct_books <= selection_count),

    -- The gap between the oldest and the newest leg. Zero for a single-book
    -- finding whose legs came off one payload.
    observed_spread_seconds     DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_observed_spread_range
                                                 CHECK (observed_spread_seconds >= 0.0
                                                        AND observed_spread_seconds < 'Infinity'::double precision),

    -- The stalest leg's age at the record's anchor. Negative is permitted for
    -- 00003's clock-skew reason, restated on ev_signals.quote_age_seconds.
    oldest_leg_age_seconds      DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_oldest_leg_age_finite
                                                 CHECK (oldest_leg_age_seconds > '-Infinity'::double precision
                                                        AND oldest_leg_age_seconds < 'Infinity'::double precision),

    -- ArbitrageRef.ObservedAt: the OLDEST leg's instant, not the newest. That is
    -- ArbitrageRef's own definition and it is the conservative one -- an
    -- opportunity is exactly as fresh as its stalest leg -- so the finding is
    -- time-stamped by its weakest component rather than flattered by its
    -- strongest. Part of the natural key.
    observed_at                 TIMESTAMPTZ      NOT NULL
                                                 CONSTRAINT arbitrage_signals_observed_at_sane
                                                 CHECK (observed_at > '1900-01-01T00:00:00Z'),

    -- THE THIRD COMPONENT OF THE NATURAL KEY, and the reason this table can be
    -- replayed idempotently at all.
    --
    -- (market_id, observed_at) is NOT unique: a market with several books can
    -- yield more than one under-round leg combination at the same instant, and a
    -- detector that reports the best two rather than only the best produces two
    -- rows that differ only in their legs. The fingerprint is a deterministic
    -- digest over the legs -- the writer's contract is documented in
    -- queries/analytics.sql -- so two findings with the same legs collapse and two
    -- with different legs do not.
    --
    -- Constrained to a lowercase hex digest so the shape is checkable; the
    -- specific hash is the writer's business, but it must be a PURE FUNCTION OF
    -- THE LEGS with no clock, no random and no map-iteration order in it, or
    -- replay stops being idempotent. Phase 12 must use the same function.
    legs_fingerprint            TEXT             NOT NULL
                                                 CONSTRAINT arbitrage_signals_legs_fingerprint_shape
                                                 CHECK (legs_fingerprint ~ '^[0-9a-f]{16,64}$'),

    -- The bounds that were applied. See the discipline note in the table header:
    -- declared and stored, never magic numbers in the detector.
    max_leg_age_seconds         DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_max_leg_age_positive
                                                 CHECK (max_leg_age_seconds > 0.0
                                                        AND max_leg_age_seconds < 'Infinity'::double precision),
    max_observed_spread_seconds DOUBLE PRECISION NOT NULL
                                                 CONSTRAINT arbitrage_signals_max_spread_positive
                                                 CHECK (max_observed_spread_seconds > 0.0
                                                        AND max_observed_spread_seconds < 'Infinity'::double precision),

    -- The stored finding must satisfy the stored bounds. This is the constraint
    -- that makes "we applied the discipline" a database fact rather than a claim
    -- in a code comment, and it is why the bounds are on the row.
    CONSTRAINT arbitrage_signals_within_own_bounds
        CHECK (oldest_leg_age_seconds <= max_leg_age_seconds
               AND observed_spread_seconds <= max_observed_spread_seconds),

    detected_at                 TIMESTAMPTZ      NOT NULL
                                                 CONSTRAINT arbitrage_signals_detected_at_sane
                                                 CHECK (detected_at > '1900-01-01T00:00:00Z'),

    created_at                  TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at                  TIMESTAMPTZ      NOT NULL DEFAULT now(),

    CONSTRAINT arbitrage_signals_pkey PRIMARY KEY (id),

    CONSTRAINT arbitrage_signals_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE RESTRICT,

    CONSTRAINT arbitrage_signals_line_rule
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN line IS NULL
                   WHEN 'futures'     THEN line IS NULL
                   WHEN 'spread'      THEN line IS NOT NULL
                   WHEN 'total'       THEN line IS NOT NULL AND line > 0
                   WHEN 'player_prop' THEN TRUE
                   ELSE FALSE
               END),

    -- The replay key. Declared as a table constraint rather than a bare unique
    -- index because this is a plain table and there is no DESC column to express,
    -- so the constraint form -- which is what ON CONFLICT ON CONSTRAINT can name
    -- -- costs nothing and reads better.
    CONSTRAINT arbitrage_signals_natural_key
        UNIQUE (market_id, observed_at, legs_fingerprint)
);

COMMENT ON TABLE arbitrage_signals IS
    'CLAUDE.md §6 Analytics: one cross-book (or single-book under-round) arbitrage finding. Plain table, not a hypertable, because TimescaleDB does not support a foreign key targeting a hypertable and arbitrage_signal_legs must reference this. Derived and recomputable: upsert on (market_id, observed_at, legs_fingerprint).';
COMMENT ON COLUMN arbitrage_signals.line IS
    'The line every leg was quoted at, in the market''s HOME frame -- unlike prices.line and ev_signals.line, which are in the selection''s own frame. An arbitrage spans both sides, so it has no single selection whose perspective it could take.';
COMMENT ON COLUMN arbitrage_signals.implied_sum IS
    'S = SUM 1/d over the legs, under-round by construction. BookingPercentage, Overround and Vig are exact single-operation functions of this and are not stored -- five columns for one number is five ways to disagree.';
COMMENT ON COLUMN arbitrage_signals.distinct_books IS
    'How many different books the legs span. ONE IS LEGAL and is the stronger finding: a single book''s own under-round market has no cross-book execution race.';
COMMENT ON COLUMN arbitrage_signals.observed_at IS
    'The OLDEST leg''s instant, per ArbitrageRef''s own definition. An opportunity is exactly as fresh as its stalest leg, so the finding is stamped by its weakest component.';
COMMENT ON COLUMN arbitrage_signals.oldest_leg_age_seconds IS
    'Age of the stalest leg. Load-bearing, not decoration: the phase-4 gate found most cross-book "arbitrage" to be one book not having moved yet, and this is the number that lets a reader judge that without trusting a threshold was applied.';
COMMENT ON COLUMN arbitrage_signals.legs_fingerprint IS
    'Deterministic digest over the legs. (market_id, observed_at) is not unique -- one market can yield several under-round combinations at one instant -- so this is what makes replay idempotent. Must be a pure function of the legs: no clock, no random, no map-iteration order. Phase 12 must use the same function.';

-- Ranked, time-bounded reads: "the best live arbitrage right now". All-DESC for
-- the keyset reason spelled out on ev_signals_rank_idx. (return_fraction,
-- observed_at, id) is total because id is the primary key.
CREATE INDEX arbitrage_signals_rank_idx
    ON arbitrage_signals (return_fraction DESC, observed_at DESC, id DESC);

-- Per-league ranking, for the same reason ev_signals has two ranking indexes: a
-- query filtered by league cannot use an index that is not led by it.
CREATE INDEX arbitrage_signals_league_rank_idx
    ON arbitrage_signals (league_id, return_fraction DESC, observed_at DESC, id DESC);

-- The per-market history panel, newest first. Mirrors 00007's
-- market_suspensions_market_idx.
CREATE INDEX arbitrage_signals_market_idx
    ON arbitrage_signals (market_id, observed_at DESC);

CREATE TRIGGER arbitrage_signals_set_updated_at
    BEFORE UPDATE ON arbitrage_signals
    FOR EACH ROW EXECUTE FUNCTION analytics_set_updated_at();


-- -----------------------------------------------------------------------------
-- arbitrage_signal_legs -- one row per outcome of one finding.
--
-- ON DELETE CASCADE, which is the FIRST cascade in this schema outside 00007's
-- market_suspensions, and it is deliberate rather than careless. 00002 refuses
-- CASCADE down the identity spine because "one DELETE against a stale league
-- silently destroys every price ever observed under it" -- the child there is
-- evidence that outlives its parent. Here the child is a PART of its parent: a
-- leg has no meaning without the finding it belongs to, the finding is derived
-- and recomputable, and a recompute that produced a different leg set must be
-- able to replace the old one atomically. RESTRICT would make that a two-step
-- delete-then-insert with an ordering hazard, and would leave orphan legs
-- referencing nothing if it were ever got wrong.
--
-- The FKs to selections and books stay RESTRICT: those ARE the identity spine.
-- -----------------------------------------------------------------------------
CREATE TABLE arbitrage_signal_legs (
    signal_id     UUID             NOT NULL
                                   REFERENCES arbitrage_signals (id) ON DELETE CASCADE,

    -- Position in ArbitrageRef.Legs, which is selection DISPLAY order. Stored
    -- rather than re-derived so the API renders the legs in the order the
    -- detector saw them without joining selections for a role ordering.
    leg_index     INTEGER          NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_index_range
                                   CHECK (leg_index >= 0 AND leg_index < 64),

    selection_id  TEXT             NOT NULL
                                   REFERENCES selections (id) ON DELETE RESTRICT
                                   CONSTRAINT arbitrage_signal_legs_selection_id_charset
                                   CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.SelectionRole. Denormalised from selections for the same reason
    -- leg_index is stored: rendering a leg should not require a join. Not pinned
    -- by a composite FK, because selections has no UNIQUE (id, role) to point at
    -- and adding one is a change to 00002 that this migration does not own.
    role          TEXT             NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_role_defined
                                   CHECK (role IN ('home', 'away', 'draw',
                                                   'over', 'under', 'outright')),

    book_id       TEXT             NOT NULL
                                   REFERENCES books (id) ON DELETE RESTRICT
                                   CONSTRAINT arbitrage_signal_legs_book_id_charset
                                   CHECK (book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    decimal_odds  DOUBLE PRECISION NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_decimal_odds_range
                                   CHECK (decimal_odds > 1.0 AND decimal_odds <= 100000.0),

    -- This leg's own line, in the SELECTION's frame (contrast the parent's
    -- home-frame line). Same semantics as prices.line.
    line          DOUBLE PRECISION
                                   CONSTRAINT arbitrage_signal_legs_line_finite
                                   CHECK (line IS NULL
                                          OR (line > '-Infinity'::double precision
                                              AND line < 'Infinity'::double precision)),

    -- The share of total outlay this leg takes, (1/d_i)/S. Strictly inside
    -- (0, 1): a leg taking none of the outlay or all of it is not part of an
    -- arbitrage. The legs of one finding sum to 1, which is NOT checked here --
    -- a CHECK cannot see sibling rows, and the sum is a float sum whose exact
    -- value depends on addition order. The parent's implied_sum is the checkable
    -- form of the same fact.
    stake_fraction DOUBLE PRECISION NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_stake_fraction_range
                                   CHECK (stake_fraction > 0.0 AND stake_fraction < 1.0),

    observed_at   TIMESTAMPTZ      NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_observed_at_sane
                                   CHECK (observed_at > '1900-01-01T00:00:00Z'),

    -- Negative permitted, per the clock-skew argument on ev_signals.
    age_seconds   DOUBLE PRECISION NOT NULL
                                   CONSTRAINT arbitrage_signal_legs_age_finite
                                   CHECK (age_seconds > '-Infinity'::double precision
                                          AND age_seconds < 'Infinity'::double precision),

    created_at    TIMESTAMPTZ      NOT NULL DEFAULT now(),

    -- No updated_at and no trigger: a leg is replaced with its parent, never
    -- edited in place. The repo convention is "every table gets created_at;
    -- MUTABLE tables also get updated_at", and 00003 makes the same call.
    CONSTRAINT arbitrage_signal_legs_pkey PRIMARY KEY (signal_id, leg_index),

    -- One leg per outcome. Two legs of one finding on the same selection would be
    -- the same side backed twice, which is not an arbitrage.
    CONSTRAINT arbitrage_signal_legs_one_per_selection UNIQUE (signal_id, selection_id)
);

COMMENT ON TABLE arbitrage_signal_legs IS
    'One outcome of one arbitrage finding: which book, at what price and line, taking what share of the outlay, observed when. ON DELETE CASCADE from arbitrage_signals because a leg is a PART of its parent, not evidence that outlives it -- contrast 00002''s refusal to cascade down the identity spine.';
COMMENT ON COLUMN arbitrage_signal_legs.stake_fraction IS
    'Share of total outlay, (1/d_i)/S. The legs of one finding sum to 1; that is not CHECKed here because a CHECK cannot see sibling rows -- the parent''s implied_sum is the checkable form of the same fact.';
COMMENT ON COLUMN arbitrage_signal_legs.line IS
    'This leg''s line in the SELECTION''s frame, unlike the parent''s home-frame line.';

-- The referencing side of the FK, and the join every read of a finding performs.
-- Without it, deleting a parent (which CASCADE does on every replay that changes
-- a leg set) is a sequential scan of this table.
CREATE INDEX arbitrage_signal_legs_signal_idx
    ON arbitrage_signal_legs (signal_id, leg_index);

-- "Which findings has this book been part of" -- the book-disagreement view, and
-- the referencing side of the book FK.
CREATE INDEX arbitrage_signal_legs_book_idx
    ON arbitrage_signal_legs (book_id, observed_at DESC);


-- =============================================================================
-- steam_signals  --  THE ONE GENUINELY NEW DETECTOR
-- =============================================================================
--
-- CLAUDE.md §3, on what Flink will eventually do: "Steam detection -- hopping
-- window over line-movement velocity, keyed by market, ACROSS BOOKS."
--
-- ---------------------------------------------------------------------------
-- WHAT A STEAM MOVE IS, AND WHY THE SCHEMA LOOKS LIKE THIS
-- ---------------------------------------------------------------------------
-- A steam move is not "a line moved fast". A line moves fast for all sorts of
-- reasons -- an injury report, a scoring play, ordinary noise on a thin market --
-- and a velocity threshold alone will fire on every one of them. What makes a
-- move STEAM is that it is CORRELATED ACROSS BOOKS: sharp money hits one book,
-- that book moves first, and the others follow within a lag because they are
-- watching the same signal.
--
-- The synthetic provider was built to make exactly this detectable, and its
-- parameters are the specification the detector is written against.
-- internal/ingest/provider/synthetic/noise.go carries `steamBlockSteps`,
-- `steamProbability`, `steamAmplitude` and `steamMinAbsZ`, and the model applies a
-- per-book view lag of up to `maxBookLag` base steps -- about 90 seconds.
-- model_test.go's TestSteamMovesPropagateToBooksWithLag proves the propagation.
--
-- THAT LAG IS THE SIGNAL. Ordinary drift is uncorrelated across books: each book
-- wanders on its own noise, so a fast move at book A says nothing about book B.
-- A steam move is one move seen at several books with a lag. So this table
-- records not just the velocity but the STRUCTURE of the move:
--
--     lead_book_id            which book moved first
--     lead_moved_at           when
--     followers               which books followed, and with what lag each
--     follower_count          how many
--     cross_book_correlation  the agreement statistic that separates the two
--
-- A detector that stored only `velocity` would be indistinguishable from a
-- threshold on drift, and both of the phase-9 gate items -- FIRES on a generated
-- steam move, does NOT fire on ordinary drift -- would be unfalsifiable from
-- storage.
--
-- ---------------------------------------------------------------------------
-- VELOCITY IS IN IMPLIED PROBABILITY, NOT IN DECIMAL ODDS. NOT NEGOTIABLE.
-- ---------------------------------------------------------------------------
-- Decimal odds are NONLINEAR in probability: d = 1/p. A move of 0.10 decimal
-- points is 0.045 probability points at d = 1.50 and 0.001 probability points at
-- d = 10.00 -- a factor of forty-five. A fixed decimal-odds threshold therefore
-- means something completely different at different prices: it would fire
-- constantly on longshots, where a tick of decimal odds is nearly no probability
-- at all, and almost never on short favourites, where a real steam move barely
-- shifts the decimal.
--
-- So velocity_probability_per_minute and delta_probability are both in
-- PROBABILITY POINTS, and the thresholds compared against them are too. Any
-- reimplementation -- phase 12's Flink SQL job included -- must convert to
-- implied probability BEFORE differencing. A Flink job that windows over
-- `decimal_odds` directly will produce a different row set and will look like a
-- disagreement about steam when it is a disagreement about units.
--
-- Whether the probabilities are DEVIGGED before differencing is a separate
-- decision and it is recorded per row in devig_method, with 'none' as a legal
-- value: devigging needs the full outcome set at one book at one instant, which a
-- per-book velocity series may not have, so the detector may legitimately window
-- over raw implied probability. What it may not do is leave the choice
-- unrecorded.
--
-- ---------------------------------------------------------------------------
-- THE WINDOW IS HOPPING, AND BOTH EDGES ARE STORED
-- ---------------------------------------------------------------------------
-- A hopping window has two parameters -- LENGTH and HOP -- and windows overlap.
-- One market can therefore be reported by several overlapping windows, which is
-- correct and is why (market_id, selection_id, window_start, window_end) is the
-- natural key rather than (market_id, selection_id) plus a timestamp: two
-- overlapping windows are two findings, not a duplicate.
--
-- window_seconds and hop_seconds are stored on every row rather than assumed from
-- config, for the ev_signals reason: a change in window length changes the row set
-- and an unrecorded window makes that change indistinguishable from a change in
-- the market. Flink's TUMBLE/HOP take exactly these two parameters, so a row here
-- names the HOP(...) arguments phase 12 must use.
--
-- WINDOW BOUNDS ARE HALF-OPEN, [window_start, window_end), matching Flink's
-- window semantics and prices.sql's history window. Stating it is not pedantry:
-- with closed bounds a quote on a boundary belongs to two adjacent windows and is
-- counted twice, which double-counts exactly the fast movements the detector is
-- looking for.
--
-- ---------------------------------------------------------------------------
-- PARTITIONED ON window_end
-- ---------------------------------------------------------------------------
-- The closing edge, because that is the instant at which the finding becomes
-- knowable and because it is the watermark boundary Flink emits a windowed result
-- at. It is in event time, it is in the natural key, and it satisfies the
-- hypertable rule -- see this migration's header for why detected_at cannot.
-- ---------------------------------------------------------------------------
CREATE TABLE steam_signals (
    market_id                       TEXT             NOT NULL
                                                     CONSTRAINT steam_signals_market_id_charset
                                                     CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    market_type                     TEXT             NOT NULL
                                                     CONSTRAINT steam_signals_market_type_defined
                                                     CHECK (market_type IN ('moneyline', 'spread', 'total',
                                                                            'player_prop', 'futures')),
    league_id                       TEXT             NOT NULL
                                                     REFERENCES leagues (id) ON DELETE RESTRICT
                                                     CONSTRAINT steam_signals_league_id_charset
                                                     CHECK (league_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- THE SIDE THAT MOVED, and the reason selection_id is in the natural key
    -- rather than only market_id. CLAUDE.md §3 says steam is "keyed by market",
    -- and a market-only key is not enough: steam is DIRECTIONAL. Money coming in
    -- on the home side moves the home price one way and the away price the other,
    -- and reporting "market mkt-123 steamed" without saying which side is a
    -- signal a user cannot act on. Keying by market alone would additionally
    -- collapse the two sides of one move into one row and silently drop half the
    -- findings -- see the header's warning to phase 12.
    selection_id                    TEXT             NOT NULL
                                                     REFERENCES selections (id) ON DELETE RESTRICT
                                                     CONSTRAINT steam_signals_selection_id_charset
                                                     CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- The hopping window, half-open [start, end). Both edges are event-time
    -- instants derived from prices.observed_at.
    window_start                    TIMESTAMPTZ      NOT NULL
                                                     CONSTRAINT steam_signals_window_start_sane
                                                     CHECK (window_start > '1900-01-01T00:00:00Z'),
    -- THE PARTITIONING COLUMN.
    window_end                      TIMESTAMPTZ      NOT NULL
                                                     CONSTRAINT steam_signals_window_end_sane
                                                     CHECK (window_end > '1900-01-01T00:00:00Z'),
    CONSTRAINT steam_signals_window_ordered
        CHECK (window_end > window_start),

    -- The window's two parameters. Both stored so the row names the HOP(...)
    -- arguments a reimplementation must use. hop <= length, or the "hopping"
    -- window is a gapped one that skips data between windows.
    window_seconds                  DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_window_seconds_positive
                                                     CHECK (window_seconds > 0.0
                                                            AND window_seconds < 'Infinity'::double precision),
    hop_seconds                     DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_hop_seconds_positive
                                                     CHECK (hop_seconds > 0.0
                                                            AND hop_seconds <= window_seconds),

    -- Which way the price went, from THIS SELECTION's perspective.
    --   'shorten'  implied probability ROSE   -- money came in on this side
    --   'drift'    implied probability FELL   -- money left this side
    -- The bookmaker's words rather than a signed number alone, because a UI
    -- badge reads "STEAM / shortening" and a sign does not survive translation
    -- through a template. delta_probability carries the sign and the two must
    -- agree -- checked below.
    direction                       TEXT             NOT NULL
                                                     CONSTRAINT steam_signals_direction_defined
                                                     CHECK (direction IN ('shorten', 'drift')),

    -- SIGNED change in implied probability across the window, in probability
    -- points. Bounded by (-1, 1) because it is a difference of two probabilities
    -- each in (0, 1). Never zero: a zero-delta steam move is not one.
    delta_probability               DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_delta_probability_range
                                                     CHECK (delta_probability > -1.0
                                                            AND delta_probability < 1.0
                                                            AND delta_probability <> 0.0),

    -- |delta_probability|. Stored rather than derived because it is the column the
    -- magnitude threshold and the ranking order both use, and abs() on a float is
    -- EXACT -- it clears the sign bit and touches nothing else -- so unlike the EV
    -- columns this identity CAN be enforced, and is.
    magnitude_probability_points    DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_magnitude_positive
                                                     CHECK (magnitude_probability_points > 0.0),
    CONSTRAINT steam_signals_magnitude_is_abs_delta
        CHECK (magnitude_probability_points = abs(delta_probability)),

    -- The sign of the move and the word for it must agree. Cheap, exact, and it
    -- catches the one bug that would make every steam badge in the UI point the
    -- wrong way.
    CONSTRAINT steam_signals_direction_matches_delta
        CHECK ((direction = 'shorten') = (delta_probability > 0.0)),

    -- SIGNED velocity, in PROBABILITY POINTS PER MINUTE. See the units argument in
    -- the table header -- this is not decimal odds per minute and a
    -- reimplementation that makes it so will disagree about which markets steamed.
    -- Per minute rather than per second because a probability-per-second figure
    -- for a real move is ~1e-4 and unreadable on a dashboard.
    velocity_probability_per_minute DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_velocity_finite
                                                     CHECK (velocity_probability_per_minute > '-Infinity'::double precision
                                                            AND velocity_probability_per_minute < 'Infinity'::double precision
                                                            AND velocity_probability_per_minute <> 0.0),
    CONSTRAINT steam_signals_velocity_matches_direction
        CHECK ((velocity_probability_per_minute > 0.0) = (delta_probability > 0.0)),

    -- Whether the probabilities were devigged before differencing. 'none' is a
    -- legal and expected value: devigging needs the full outcome set at one book
    -- at one instant, which a per-book velocity series may not have. The choice
    -- must be recorded either way -- two detectors that disagree about devigging
    -- produce different velocities on the same data.
    devig_method                    TEXT             NOT NULL
                                                     CONSTRAINT steam_signals_devig_method_defined
                                                     CHECK (devig_method IN ('none', 'multiplicative', 'additive',
                                                                             'power', 'shin')),

    -- ------------------------------------------------------------------------
    -- THE CROSS-BOOK STRUCTURE. This is what makes it steam rather than drift.
    -- ------------------------------------------------------------------------

    -- The book that moved FIRST. In the synthetic model this is the book with the
    -- smallest view lag; against a real feed it is whichever book the sharp money
    -- reached. Not necessarily the reference book.
    lead_book_id                    TEXT             NOT NULL
                                                     REFERENCES books (id) ON DELETE RESTRICT
                                                     CONSTRAINT steam_signals_lead_book_id_charset
                                                     CHECK (lead_book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- When the lead book's move was OBSERVED -- a provider instant, inside the
    -- window's half-open bounds.
    lead_moved_at                   TIMESTAMPTZ      NOT NULL
                                                     CONSTRAINT steam_signals_lead_moved_at_sane
                                                     CHECK (lead_moved_at > '1900-01-01T00:00:00Z'),
    CONSTRAINT steam_signals_lead_moved_within_window
        CHECK (lead_moved_at >= window_start AND lead_moved_at < window_end),

    -- The books that followed, as a JSON array. See the migration header for why
    -- this is JSONB and not a child table (short version: a hypertable cannot be
    -- an FK target, and an ARRAY<ROW<...>> is the shape a Flink SQL job emits).
    --
    -- ELEMENT SHAPE, which phase 12 must reproduce exactly:
    --
    --     {"book_id":          TEXT,    a books(id)
    --      "moved_at":         RFC3339 UTC, the follower's observation instant
    --      "lag_seconds":      NUMBER,  moved_at - lead_moved_at, >= 0
    --      "delta_probability":NUMBER}  the follower's own signed move
    --
    -- Ordered by lag_seconds ascending, so the array reads as the propagation
    -- order. There is no FK from inside a JSONB document, so book_id here is NOT
    -- database-enforced -- that is the cost of the shape, stated rather than
    -- glossed, and the writer is responsible for it.
    followers                       JSONB            NOT NULL
                                                     CONSTRAINT steam_signals_followers_is_array
                                                     CHECK (jsonb_typeof(followers) = 'array'),

    -- How many books followed. Denormalised out of the array so the
    -- min-followers threshold is an indexable predicate rather than a function
    -- call on every row, and pinned to the array's length so it cannot drift.
    follower_count                  INTEGER          NOT NULL
                                                     CONSTRAINT steam_signals_follower_count_positive
                                                     CHECK (follower_count >= 1 AND follower_count <= 256),
    CONSTRAINT steam_signals_follower_count_matches_array
        CHECK (follower_count = jsonb_array_length(followers)),

    -- Lead + followers. At least 2: a "steam move" one book made alone is drift,
    -- and this constraint is the schema-level statement of the second gate item.
    participating_books             INTEGER          NOT NULL
                                                     CONSTRAINT steam_signals_participating_books_range
                                                     CHECK (participating_books >= 2),
    CONSTRAINT steam_signals_participating_is_lead_plus_followers
        CHECK (participating_books = follower_count + 1),

    -- THE STATISTIC THAT SEPARATES STEAM FROM DRIFT. How strongly the
    -- participating books' moves agreed within the window, in [-1, 1]. A high
    -- positive value is books moving together, which is steam; a value near zero
    -- is books wandering independently, which is drift. The detector's threshold
    -- is on this AND on velocity AND on follower count, never on velocity alone.
    cross_book_correlation          DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_correlation_range
                                                     CHECK (cross_book_correlation >= -1.0
                                                            AND cross_book_correlation <= 1.0),

    -- ------------------------------------------------------------------------
    -- THE THRESHOLDS IN FORCE. Same argument as ev_signals: without them a change
    -- in signal volume is unattributable and the phase-12 diff is meaningless.
    -- Five of them, because steam is a conjunction of five conditions and each one
    -- is what stops a different false positive:
    --
    --   velocity     the move was fast          (vs. a slow grind)
    --   magnitude    the move was big enough    (vs. rounding noise on a thin book)
    --   correlation  the books agreed           (vs. independent drift)
    --   followers    enough books agreed        (vs. one book's outage)
    --   lag          they followed soon enough  (vs. two unrelated moves an hour apart)
    --
    -- The last one is the parameter the synthetic model pins: per-book view lag is
    -- up to maxBookLag base steps, about 90 seconds, so a follower window shorter
    -- than that will miss real propagation and the detector will fail the "fires
    -- on a generated steam move" gate item.
    -- ------------------------------------------------------------------------
    threshold_velocity              DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_threshold_velocity_positive
                                                     CHECK (threshold_velocity > 0.0
                                                            AND threshold_velocity < 'Infinity'::double precision),
    threshold_magnitude             DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_threshold_magnitude_positive
                                                     CHECK (threshold_magnitude > 0.0 AND threshold_magnitude < 1.0),
    threshold_correlation           DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_threshold_correlation_range
                                                     CHECK (threshold_correlation >= -1.0
                                                            AND threshold_correlation <= 1.0),
    min_followers                   INTEGER          NOT NULL
                                                     CONSTRAINT steam_signals_min_followers_positive
                                                     CHECK (min_followers >= 1),
    max_follower_lag_seconds        DOUBLE PRECISION NOT NULL
                                                     CONSTRAINT steam_signals_max_follower_lag_positive
                                                     CHECK (max_follower_lag_seconds > 0.0
                                                            AND max_follower_lag_seconds < 'Infinity'::double precision),

    -- The stored finding must satisfy the stored thresholds, exactly as
    -- arbitrage_signals_within_own_bounds does. This is what makes "the detector
    -- applied its own rules" a database fact.
    CONSTRAINT steam_signals_meets_own_thresholds
        CHECK (abs(velocity_probability_per_minute) >= threshold_velocity
               AND magnitude_probability_points     >= threshold_magnitude
               AND cross_book_correlation           >= threshold_correlation
               AND follower_count                   >= min_followers),

    detected_at                     TIMESTAMPTZ      NOT NULL
                                                     CONSTRAINT steam_signals_detected_at_sane
                                                     CHECK (detected_at > '1900-01-01T00:00:00Z'),

    created_at                      TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at                      TIMESTAMPTZ      NOT NULL DEFAULT now(),

    CONSTRAINT steam_signals_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE RESTRICT
);

COMMENT ON TABLE steam_signals IS
    'CLAUDE.md §3: hopping window over line-movement velocity, keyed by market, ACROSS BOOKS. The one genuinely new phase-9 detector. Velocity is in IMPLIED PROBABILITY per minute, never decimal odds -- decimal is nonlinear in probability so a fixed decimal threshold means different things at different prices. Hypertable partitioned on window_end.';
COMMENT ON COLUMN steam_signals.selection_id IS
    'The side that moved. Steam is DIRECTIONAL, so the key is (market, selection, window) and not (market, window): keying by market alone collapses the two sides of one move into one row and drops half the findings.';
COMMENT ON COLUMN steam_signals.window_start IS
    'Inclusive lower edge of the hopping window. Bounds are half-open [start, end) -- with closed bounds a boundary quote falls in two adjacent windows and double-counts the movement the detector is looking for.';
COMMENT ON COLUMN steam_signals.window_end IS
    'Exclusive upper edge, and the partitioning column: the instant the finding becomes knowable, which is also the watermark boundary a Flink HOP window emits at.';
COMMENT ON COLUMN steam_signals.velocity_probability_per_minute IS
    'Signed velocity in PROBABILITY POINTS per minute. Not decimal odds: a 0.10 decimal move is 0.045 probability points at d=1.50 and 0.001 at d=10.00, so a fixed decimal threshold fires constantly on longshots and never on favourites.';
COMMENT ON COLUMN steam_signals.lead_book_id IS
    'The book that moved first. In the synthetic model the book with the smallest view lag (noise.go, maxBookLag ~90s); against a real feed, whichever book the sharp money reached.';
COMMENT ON COLUMN steam_signals.followers IS
    'JSON array of {book_id, moved_at, lag_seconds, delta_probability}, ordered by lag ascending. book_id here is NOT foreign-key enforced -- there is no FK from inside a JSONB document. That is the stated cost of the shape a Flink ARRAY<ROW<...>> sink produces.';
COMMENT ON COLUMN steam_signals.cross_book_correlation IS
    'How strongly the participating books'' moves agreed, in [-1, 1]. THE statistic that separates steam from drift: books moving together is steam, books wandering independently is drift. Thresholded alongside velocity, magnitude and follower count -- never velocity alone.';
COMMENT ON COLUMN steam_signals.max_follower_lag_seconds IS
    'How long a book may take to follow and still count. The synthetic model applies per-book view lag of up to maxBookLag base steps (~90s), so a shorter window here will miss real propagation.';

-- 7-DAY CHUNKS. Steam is RARE BY CONSTRUCTION -- the synthetic model gates it on
-- steamProbability and steamMinAbsZ, and against a real feed a genuine
-- correlated cross-book move is a handful of events per slate. The design
-- envelope is 10^2..10^3 rows/day, three to four orders below ev_signals, so a
-- 7-day chunk is single-digit MB and there is no memory argument to make. The
-- interval is chosen on the OTHER axis instead: alert reads span days ("what
-- steamed this week"), and 7-day chunks make that one chunk instead of seven.
--
-- 52 chunks/year is also the smallest chunk count that keeps `make query-plans`
-- readable, which matters more here than partition pruning does at this size.
SELECT create_hypertable(
    'steam_signals',
    by_range('window_end', INTERVAL '7 days'),
    create_default_indexes => FALSE
);

-- THE NATURAL KEY. Two overlapping hopping windows over the same market and side
-- are two findings, not a duplicate -- so both window edges are in the key. The
-- partitioning column (window_end) is present, as the engine requires, and
-- nothing here comes from a clock of ours, as replay requires.
CREATE UNIQUE INDEX steam_signals_natural_key_idx
    ON steam_signals (market_id, selection_id, window_start, window_end DESC);

COMMENT ON INDEX steam_signals_natural_key_idx IS
    'The replay key: (market, side, window). Both window edges are present because a hopping window overlaps its neighbour and two overlapping windows are two findings. Also the ON CONFLICT arbiter for idempotent recomputation.';

-- "What steamed recently", newest first, all-DESC for the keyset reason on
-- ev_signals_rank_idx. Ranked by RECENCY rather than by magnitude, deliberately:
-- a steam alert is only actionable while the followers are still catching up, so
-- an hour-old bigger move is worth less than a fresh smaller one. The magnitude
-- is a filter (threshold_magnitude), not the sort.
--
-- (window_end, market_id, selection_id) is total because
-- (market_id, selection_id, window_start, window_end) is unique and, for a fixed
-- window length, window_start is a function of window_end.
CREATE INDEX steam_signals_recent_idx
    ON steam_signals (window_end DESC, market_id DESC, selection_id DESC);

CREATE INDEX steam_signals_league_recent_idx
    ON steam_signals (league_id, window_end DESC, market_id DESC, selection_id DESC);

COMMENT ON INDEX steam_signals_recent_idx IS
    'The alert feed, newest first. Ranked by recency and not by magnitude: a steam alert is actionable only while the followers are still catching up, so a fresh small move beats an hour-old large one.';

CREATE TRIGGER steam_signals_set_updated_at
    BEFORE UPDATE ON steam_signals
    FOR EACH ROW EXECUTE FUNCTION analytics_set_updated_at();


-- =============================================================================
-- wager_leg_clv  --  the persisted form of odds.CLVResult
-- =============================================================================
--
-- odds.CLVResult's own doc comment names this table's owner and its readers:
-- "the settle service writes one per graded leg, the API serves it, and the
-- phase-12 Flink job reproduces it." This table exists to hold every field of
-- that struct, so that the third clause is possible.
--
-- ---------------------------------------------------------------------------
-- ONE ROW PER GRADED LEG. leg_id IS THE PRIMARY KEY.
-- ---------------------------------------------------------------------------
-- Idempotency comes free from that: a leg has exactly one placement price and
-- exactly one close, so recomputation is an upsert on the primary key with no
-- fingerprint and no compound key to get wrong. It is also why this is a plain
-- table -- a hypertable's primary key must contain its partitioning column, and
-- adding a time column to this key would destroy the property.
--
-- ABSENCE IS MEANINGFUL. A graded leg with no row here is a leg whose CLV could
-- not be computed, and there are exactly four reasons, all of them enforced by
-- odds.EvaluateCLV rather than invented here:
--
--   1. The closing snapshot was INCOMPLETE. Devigging needs the whole outcome
--      set, so a close missing one selection is not a close.
--   2. The OUTCOME SET CHANGED between placement and close (a three-way market
--      that lost its draw). ErrCLVOutcomeSetChanged.
--   3. The close would PRECEDE the take. ErrCLVClosingBeforeTaken.
--   4. Every candidate closing quote was observed while the market was
--      SUSPENDED, so there was no eligible close. See the closing-snapshot query
--      in queries/analytics.sql.
--
-- Writing a row with nulls for these cases would put "we could not measure it"
-- and "it measured zero" in the same shape, and a leaderboard cannot tell them
-- apart. So the settle service writes nothing and the API reports the leg as
-- unmeasured.
--
-- The FIFTH case is different and DOES get a row: a LINE MOVE. See below.
--
-- ---------------------------------------------------------------------------
-- line_moved: STORED, SERVED, AND NEVER RANKED
-- ---------------------------------------------------------------------------
-- EvaluateCLVAcrossLineMove exists so a user can see what happened to a bet
-- whose market moved from -3 to -3.5, and its doc comment is unambiguous about
-- what the number is worth: "A spread of -3 and a spread of -3.5 differ by
-- whatever probability mass sits on a three-point margin, and that mass is in the
-- result with no way to separate it out. Show it next to the two lines in a user
-- interface; NEVER RANK ANYONE BY IT. AggregateCLV enforces the second half of
-- that sentence."
--
-- This schema enforces it too, in the only way a schema can: line_moved is a
-- column, the leaderboard query filters on it, and the filter is not optional
-- because it is written into the statement rather than passed as a parameter.
-- Both `taken_line` and `closing_line` are stored so the UI can show the two
-- lines side by side, which is the display EvaluateCLVAcrossLineMove is for.
--
-- ---------------------------------------------------------------------------
-- WHY user_id IS DENORMALISED HERE
-- ---------------------------------------------------------------------------
-- It is reachable through wagers, and the leaderboard would otherwise join
-- wager_leg_clv -> wagers -> group by user on every read, for a column that can
-- never change: a wager does not change hands. The denormalisation is pinned by a
-- composite FK to wagers (id, user_id), so a row claiming the wrong owner is
-- rejected -- the same trick ev_signals uses on (market_id, market_type). That
-- pairing needs a UNIQUE (id, user_id) on wagers, which 00006 does not declare,
-- so the constraint below is a plain two-column FK to wagers(id) plus a separate
-- FK to users(id) and a documented writer obligation. Recorded honestly rather
-- than claimed: adding `ALTER TABLE wagers ADD CONSTRAINT wagers_id_user_key
-- UNIQUE (id, user_id)` in a future migration would let this be enforced, and
-- that is a change to 00006's table which this migration does not own.
-- ---------------------------------------------------------------------------
CREATE TABLE wager_leg_clv (
    -- THE PRIMARY KEY. One graded leg, one close.
    --
    -- ON DELETE RESTRICT, per 00006's discipline throughout the betting domain.
    -- A CLV row is derived, but the leg it is about is a financial record, and
    -- RESTRICT here means "you cannot delete a leg while its measurement exists",
    -- which is the correct direction: delete the measurement first, deliberately.
    leg_id            TEXT             NOT NULL
                                       REFERENCES legs (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_leg_id_charset
                                       CHECK (leg_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    wager_id          TEXT             NOT NULL
                                       REFERENCES wagers (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_wager_id_charset
                                       CHECK (wager_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Denormalised from wagers; see the header for why, and for the honest note
    -- about what is and is not database-enforced.
    user_id           TEXT             NOT NULL
                                       REFERENCES users (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_user_id_charset
                                       CHECK (user_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- odds.CLVResult.Market / .Selection. Both snapshots agreed on these or
    -- evaluation would have failed, so there is one market and one selection.
    market_id         TEXT             NOT NULL
                                       CONSTRAINT wager_leg_clv_market_id_charset
                                       CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    market_type       TEXT             NOT NULL
                                       CONSTRAINT wager_leg_clv_market_type_defined
                                       CHECK (market_type IN ('moneyline', 'spread', 'total',
                                                              'player_prop', 'futures')),
    selection_id      TEXT             NOT NULL
                                       REFERENCES selections (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_selection_id_charset
                                       CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- For the per-league CLV breakdown (§6: "CLV tracking per user"; the league
    -- cut is what makes it diagnostic rather than a single number).
    league_id         TEXT             NOT NULL
                                       REFERENCES leagues (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_league_id_charset
                                       CHECK (league_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- CLVResult.TakenBook / .ClosingBook. "They are frequently different: a wager
    -- struck at one book is normally scored against a sharp reference book's
    -- close." taken_book_id is the leg's own price_book_id; closing_book_id is
    -- the reference book (ADR 0006).
    taken_book_id     TEXT             NOT NULL
                                       REFERENCES books (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_taken_book_id_charset
                                       CHECK (taken_book_id ~ '^[A-Za-z0-9._-]{1,128}$'),
    closing_book_id   TEXT             NOT NULL
                                       REFERENCES books (id) ON DELETE RESTRICT
                                       CONSTRAINT wager_leg_clv_closing_book_id_charset
                                       CHECK (closing_book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Which devig produced both fair probabilities. It MUST be the same method on
    -- both sides -- comparing a Shin-devigged take against a multiplicatively
    -- devigged close measures the difference between two devig methods, not
    -- closing line value -- which is why there is one column and not two.
    devig_method      TEXT             NOT NULL
                                       CONSTRAINT wager_leg_clv_devig_method_defined
                                       CHECK (devig_method IN ('multiplicative', 'additive',
                                                               'power', 'shin')),

    -- CLVResult.Line / .ClosingLine, both in the SNAPSHOT's frame (domain.Line
    -- semantics, NULL is NoLine). "They are equal unless LineMoved is set" --
    -- enforced below.
    taken_line        DOUBLE PRECISION
                                       CONSTRAINT wager_leg_clv_taken_line_finite
                                       CHECK (taken_line IS NULL
                                              OR (taken_line > '-Infinity'::double precision
                                                  AND taken_line < 'Infinity'::double precision)),
    closing_line      DOUBLE PRECISION
                                       CONSTRAINT wager_leg_clv_closing_line_finite
                                       CHECK (closing_line IS NULL
                                              OR (closing_line > '-Infinity'::double precision
                                                  AND closing_line < 'Infinity'::double precision)),

    -- CLVResult.TakenAt / .ClosedAt, both UTC provider observation instants.
    -- "ClosedAt is never before TakenAt" -- EvaluateCLV returns
    -- ErrCLVClosingBeforeTaken otherwise, so the schema can assert it. Note that
    -- this is safe where 00003 declined the analogous check: both values come
    -- from the SAME clock (the provider's, via prices.observed_at), so there is
    -- no cross-clock skew to reject a legitimate row.
    taken_at          TIMESTAMPTZ      NOT NULL
                                       CONSTRAINT wager_leg_clv_taken_at_sane
                                       CHECK (taken_at > '1900-01-01T00:00:00Z'),
    closed_at         TIMESTAMPTZ      NOT NULL
                                       CONSTRAINT wager_leg_clv_closed_at_sane
                                       CHECK (closed_at > '1900-01-01T00:00:00Z'),
    CONSTRAINT wager_leg_clv_close_not_before_take
        CHECK (closed_at >= taken_at),

    -- CLVResult.TakenFair / .ClosingFair and their decimal forms 1/p. "These are
    -- FAIR prices, not the quotes the book displayed" -- the whole discipline of
    -- the CLV package is that it operates on devigged probabilities, and
    -- odds.NewFairMarketSnapshot mechanically refuses vigged input.
    taken_fair        DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_taken_fair_range
                                       CHECK (taken_fair > 0.0 AND taken_fair < 1.0),
    closing_fair      DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_closing_fair_range
                                       CHECK (closing_fair > 0.0 AND closing_fair < 1.0),
    taken_price       DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_taken_price_range
                                       CHECK (taken_price > 1.0 AND taken_price <= 100000.0),
    closing_price     DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_closing_price_range
                                       CHECK (closing_price > 1.0 AND closing_price <= 100000.0),

    -- CLVResult.ProbabilityCLV = ClosingFair - TakenFair, in probability points.
    --
    -- THE IDENTITY IS CHECKED, and this is the one place in this file where that
    -- is safe. It is a SINGLE IEEE-754 subtraction of two stored doubles with no
    -- association-order freedom and no intermediate rounding, so PostgreSQL's
    -- float8 minus and Go's float64 minus produce bit-identical results on the
    -- same operands. Contrast ev_signals' EV and Kelly columns, which go through
    -- multi-step formulas where that guarantee does not hold and where no
    -- identity constraint is therefore attempted.
    probability_clv   DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_probability_clv_finite
                                       CHECK (probability_clv > '-Infinity'::double precision
                                              AND probability_clv < 'Infinity'::double precision),
    CONSTRAINT wager_leg_clv_probability_identity
        CHECK (probability_clv = closing_fair - taken_fair),

    -- CLVResult.PercentCLV = (TakenPrice/ClosingPrice - 1) x 100.
    --
    -- NO identity constraint, on purpose and for a reason worth stating rather
    -- than leaving to inference: this is three chained operations, and Go's
    -- odds.PercentCLV is free to fuse or reorder them differently from
    -- PostgreSQL's evaluator. The subtraction above is exact; this is not
    -- reliably so, and a constraint that rejects a correct row one time in ten
    -- thousand is worse than no constraint.
    percent_clv       DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_percent_clv_finite
                                       CHECK (percent_clv > '-Infinity'::double precision
                                              AND percent_clv < 'Infinity'::double precision),

    -- CLVResult.Magnitude = |PercentCLV|. abs() on a float is exact -- it clears
    -- the sign bit and nothing else -- so this identity IS checkable, exactly as
    -- steam_signals' magnitude is.
    magnitude         DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wager_leg_clv_magnitude_non_negative
                                       CHECK (magnitude >= 0.0),
    CONSTRAINT wager_leg_clv_magnitude_is_abs_percent
        CHECK (magnitude = abs(percent_clv)),

    -- CLVResult.Beat = ProbabilityCLV > CLVTieBand, where odds.CLVTieBand is
    -- 1e-12. The literal is repeated here rather than left implicit because the
    -- boolean is what the leaderboard's beat-rate counts, and a schema that
    -- accepted a Beat inconsistent with its own delta would let two
    -- implementations disagree about who beat the close while both looking
    -- internally consistent. If odds.CLVTieBand ever changes, this constraint is
    -- the thing that fails and says so.
    beat_close        BOOLEAN          NOT NULL,
    CONSTRAINT wager_leg_clv_beat_matches_tie_band
        CHECK (beat_close = (probability_clv > 1e-12)),

    -- CLVResult.LineMoved. "Such a result is indicative only -- AggregateCLV
    -- excludes it." The flag and the two lines must agree: a row flagged
    -- line_moved whose lines are equal, or an unflagged row whose lines differ,
    -- is a writer bug. IS DISTINCT FROM rather than <> so that NULL (NoLine) on
    -- one side and a value on the other counts as a move -- which is the same
    -- rule domain.Line.Equal applies, where "an absent line matches only another
    -- absent line".
    line_moved        BOOLEAN          NOT NULL,
    CONSTRAINT wager_leg_clv_line_moved_matches_lines
        CHECK (line_moved = (taken_line IS DISTINCT FROM closing_line)),

    -- The leg's terminal status. NEVER 'pending': a CLV row is written per GRADED
    -- leg, and 00006's legs_graded_at_iff_graded makes graded_at NOT NULL exactly
    -- when status <> 'pending'.
    leg_status        TEXT             NOT NULL
                                       CONSTRAINT wager_leg_clv_leg_status_defined
                                       CHECK (leg_status IN ('won', 'lost', 'void', 'push')),

    -- odds.CLVSample.Void: "a wager that was cancelled, abandoned or otherwise
    -- given no action. It is excluded from every statistic, numerator and
    -- denominator alike."
    --
    -- DERIVED FROM leg_status AND PINNED TO IT, so the exclusion rule is a schema
    -- fact rather than a convention the aggregate query has to remember. Note
    -- what is NOT void: a PUSH. A push had action -- the price was taken, the
    -- market closed, the CLV is real -- and only the settlement returned the
    -- stake. Voiding a push would drop the sharpest bettors' most common outcome
    -- on totals.
    voided            BOOLEAN          NOT NULL,
    CONSTRAINT wager_leg_clv_voided_matches_status
        CHECK (voided = (leg_status = 'void')),

    -- When the leg was graded. Not the close, and not detection: legs.graded_at,
    -- copied so the per-user history can be paged in grading order without
    -- joining legs.
    graded_at         TIMESTAMPTZ      NOT NULL
                                       CONSTRAINT wager_leg_clv_graded_at_sane
                                       CHECK (graded_at > '1900-01-01T00:00:00Z'),

    -- When settle computed this. OUR clock. Never part of any key.
    computed_at       TIMESTAMPTZ      NOT NULL
                                       CONSTRAINT wager_leg_clv_computed_at_sane
                                       CHECK (computed_at > '1900-01-01T00:00:00Z'),

    created_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ      NOT NULL DEFAULT now(),

    CONSTRAINT wager_leg_clv_pkey PRIMARY KEY (leg_id),

    CONSTRAINT wager_leg_clv_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE RESTRICT,

    CONSTRAINT wager_leg_clv_line_rule
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN taken_line IS NULL AND closing_line IS NULL
                   WHEN 'futures'     THEN taken_line IS NULL AND closing_line IS NULL
                   WHEN 'spread'      THEN taken_line IS NOT NULL AND closing_line IS NOT NULL
                   WHEN 'total'       THEN taken_line IS NOT NULL AND taken_line > 0
                                           AND closing_line IS NOT NULL AND closing_line > 0
                   WHEN 'player_prop' THEN TRUE
                   ELSE FALSE
               END)
);

COMMENT ON TABLE wager_leg_clv IS
    'The persisted form of odds.CLVResult, one row per GRADED leg. odds/clv.go: "the settle service writes one per graded leg, the API serves it, and the phase-12 Flink job reproduces it." ABSENCE IS MEANINGFUL -- a graded leg with no row is one whose close was incomplete, whose outcome set changed, whose close preceded the take, or which had no eligible (non-suspended) closing quote.';
COMMENT ON COLUMN wager_leg_clv.closing_book_id IS
    'The book the CLOSE was read at -- normally the sharp reference book (ADR 0006), frequently different from taken_book_id.';
COMMENT ON COLUMN wager_leg_clv.devig_method IS
    'One column, not two: the same method must devig both sides. Comparing a Shin-devigged take against a multiplicatively devigged close measures the difference between two devig methods, not closing line value.';
COMMENT ON COLUMN wager_leg_clv.probability_clv IS
    'ClosingFair - TakenFair, in probability points. The identity IS enforced -- a single IEEE-754 subtraction is bit-identical in Go and PostgreSQL. percent_clv''s identity deliberately is not: three chained operations have no such guarantee.';
COMMENT ON COLUMN wager_leg_clv.beat_close IS
    'ProbabilityCLV > odds.CLVTieBand (1e-12). The dead band is in the CHECK so that two implementations cannot disagree about who beat the close while each looks internally consistent.';
COMMENT ON COLUMN wager_leg_clv.line_moved IS
    'The two snapshots were taken at different lines, so this row came from EvaluateCLVAcrossLineMove. INDICATIVE ONLY -- show the two lines side by side, never rank anyone by it. The leaderboard query hard-codes the exclusion rather than parameterising it.';
COMMENT ON COLUMN wager_leg_clv.voided IS
    'odds.CLVSample.Void -- excluded from every statistic, numerator and denominator alike. Pinned to leg_status so the exclusion is a schema fact. A PUSH is NOT void: it had action, and voiding it would drop the sharpest bettors'' most common outcome on totals.';

-- The per-user history, newest first, and the leaderboard's grouping key. Keyset
-- paging over (graded_at, leg_id) is total because leg_id is the primary key.
CREATE INDEX wager_leg_clv_user_idx
    ON wager_leg_clv (user_id, graded_at DESC, leg_id DESC);

-- THE LEADERBOARD INDEX. Partial on exactly the two exclusions AggregateCLV
-- applies, so the aggregate never reads a row it is going to discard, and so the
-- exclusions are visible in the schema rather than only in a WHERE clause
-- somebody could forget.
CREATE INDEX wager_leg_clv_countable_idx
    ON wager_leg_clv (user_id) WHERE NOT voided AND NOT line_moved;

-- Every CLV row for one wager -- the wager-detail panel, and the referencing side
-- of the wagers FK.
CREATE INDEX wager_leg_clv_wager_idx
    ON wager_leg_clv (wager_id);

-- The per-league cut of a user's CLV.
CREATE INDEX wager_leg_clv_league_idx
    ON wager_leg_clv (league_id, graded_at DESC);

COMMENT ON INDEX wager_leg_clv_countable_idx IS
    'Partial on NOT voided AND NOT line_moved -- exactly odds.AggregateCLV''s two exclusions. The aggregate never touches a row it would discard, and the exclusion rule is visible in the schema rather than only in a WHERE clause.';

CREATE TRIGGER wager_leg_clv_set_updated_at
    BEFORE UPDATE ON wager_leg_clv
    FOR EACH ROW EXECUTE FUNCTION analytics_set_updated_at();


-- =============================================================================
-- POSTCONDITION
-- =============================================================================
-- 00001, 00003 and 00007 each assert what they built. This asserts the facts
-- every phase-9 service and every generated query is entitled to assume, so a
-- wrong state is attributed HERE rather than surfacing as a mysteriously slow
-- ranked query or a replay that duplicates rows in production.
-- =============================================================================
-- +goose StatementBegin
DO $postcondition$
DECLARE
    v_dim      text;
    v_interval interval;
BEGIN
    -- ev_signals: hypertable on quote_observed_at, 1-day chunks.
    SELECT d.column_name, d.time_interval
      INTO v_dim, v_interval
      FROM timescaledb_information.dimensions d
     WHERE d.hypertable_name = 'ev_signals';

    IF v_dim IS NULL THEN
        RAISE EXCEPTION 'ev_signals was not registered as a hypertable'
            USING HINT = 'create_hypertable() returned without error but timescaledb_information.dimensions has no row.';
    END IF;
    IF v_dim <> 'quote_observed_at' THEN
        RAISE EXCEPTION 'ev_signals is partitioned on %, expected quote_observed_at', v_dim
            USING HINT = 'The partitioning column must be the PROVIDER observation instant: it is in the '
                         'natural key, which is what makes replay idempotent, and it is phase 12''s '
                         'Flink event-time attribute. detected_at cannot serve -- see migration 00009''s header.';
    END IF;
    IF v_interval IS DISTINCT FROM INTERVAL '1 day' THEN
        RAISE EXCEPTION 'ev_signals chunk interval is %, expected 1 day', v_interval::text
            USING HINT = 'Retune with set_chunk_time_interval() in a follow-up migration, not by editing this one.';
    END IF;

    -- steam_signals: hypertable on window_end, 7-day chunks.
    SELECT d.column_name, d.time_interval
      INTO v_dim, v_interval
      FROM timescaledb_information.dimensions d
     WHERE d.hypertable_name = 'steam_signals';

    IF v_dim IS NULL THEN
        RAISE EXCEPTION 'steam_signals was not registered as a hypertable';
    END IF;
    IF v_dim <> 'window_end' THEN
        RAISE EXCEPTION 'steam_signals is partitioned on %, expected window_end', v_dim;
    END IF;
    IF v_interval IS DISTINCT FROM INTERVAL '7 days' THEN
        RAISE EXCEPTION 'steam_signals chunk interval is %, expected 7 days', v_interval::text;
    END IF;

    -- The two tables that must NOT be hypertables. Asserted in the negative
    -- because the failure is silent: making arbitrage_signals a hypertable
    -- succeeds, and only the child table's foreign key fails afterwards, several
    -- statements later and attributed to the wrong line.
    IF EXISTS (SELECT 1 FROM timescaledb_information.hypertables
                WHERE hypertable_name IN ('arbitrage_signals', 'arbitrage_signal_legs', 'wager_leg_clv')) THEN
        RAISE EXCEPTION 'arbitrage_signals, its legs and wager_leg_clv must remain PLAIN tables'
            USING HINT = 'TimescaleDB does not support a foreign key whose target is a hypertable, and '
                         'wager_leg_clv''s primary key is leg_id alone. See migration 00009''s header.';
    END IF;

    -- The four replay keys. Each is what makes recomputation an upsert rather
    -- than a duplicate insert; losing one is not a performance regression, it is
    -- a table that doubles in size every time a consumer group is reset.
    IF to_regclass('ev_signals_natural_key_idx') IS NULL THEN
        RAISE EXCEPTION 'ev_signals_natural_key_idx is missing -- replay would duplicate rows';
    END IF;
    IF to_regclass('steam_signals_natural_key_idx') IS NULL THEN
        RAISE EXCEPTION 'steam_signals_natural_key_idx is missing -- replay would duplicate rows';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'arbitrage_signals_natural_key' AND contype = 'u') THEN
        RAISE EXCEPTION 'arbitrage_signals_natural_key is missing -- replay would duplicate rows';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint
                    WHERE conname = 'wager_leg_clv_pkey' AND contype = 'p') THEN
        RAISE EXCEPTION 'wager_leg_clv_pkey is missing -- one leg must have exactly one close';
    END IF;

    RAISE NOTICE 'analytics: ev_signals (1d/quote_observed_at) + steam_signals (7d/window_end) hypertables, arbitrage + clv plain, 4 replay keys installed';
END
$postcondition$;
-- +goose StatementEnd


-- +goose Down

-- -----------------------------------------------------------------------------
-- Reverses the Up exactly, and reverses nothing else.
--
-- Order matters. Child before parent, so arbitrage_signal_legs goes first --
-- although the CASCADE would take it either way, dropping it explicitly keeps
-- this readable as a line-by-line mirror. DROP TABLE on a hypertable removes the
-- hypertable, every chunk, every index and every trigger, so there is no
-- drop_hypertable() call here (00003 and 00007 both note this). The trigger
-- function goes LAST, after every trigger that depends on it is gone; reversing
-- that order fails with a dependency error.
--
-- 00007's warning applies with less force but is worth repeating in the right
-- proportion: running this Down destroys the signal history and the CLV ledger.
-- Unlike audit_log, all four tables are DERIVED and can be rebuilt by replaying
-- the Kafka topics and re-running settle's CLV pass over the graded legs -- which
-- is exactly the property the header argues for. So this Down is safe in a way
-- 00006's and 00007's are not, and it is still a review artifact rather than an
-- operational tool: the policy is forward-only.
-- -----------------------------------------------------------------------------
DROP TABLE IF EXISTS arbitrage_signal_legs;
DROP TABLE IF EXISTS arbitrage_signals;
DROP TABLE IF EXISTS wager_leg_clv;
DROP TABLE IF EXISTS steam_signals;
DROP TABLE IF EXISTS ev_signals;

DROP FUNCTION IF EXISTS analytics_set_updated_at();
