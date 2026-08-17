-- =============================================================================
-- 00006  wagers, legs, and the double-entry ledger
-- =============================================================================
--
-- CLAUDE.md §6 (Betting): "bet slip with straight, parlay, round robin, and
-- teaser support; ... stake and payout calculation; wager history; open position
-- tracking; cash-out pricing on live events."
--
-- CLAUDE.md §4 (Domain model):
--
--   "Wager / Leg -- a placed bet. Straight, parlay, round robin, teaser. Legs
--    hold the price *at placement time*, never a live reference."
--
--   "LedgerEntry -- double-entry. Every stake, payout, void, and adjustment is
--    two rows that sum to zero. Balances are derived, never stored as a mutable
--    field."
--
-- The specification for this file is `internal/domain/wager.go`, `leg.go` and
-- `ledger.go`, not a general opinion about what a betting schema looks like.
-- Every CHECK, every trigger, and every nullable column below traces to a named
-- constructor, validator, or state machine in those three files, and the
-- comments cite them so the correspondence is auditable rather than asserted.
--
-- Six tables and two views:
--
--   round_robins        one placed round robin: the selections it draws from and
--                       the per-ticket stake  (domain.RoundRobin)
--   round_robin_sizes   the combination sizes it expands by ("by 2s and 3s")
--   wagers              one ticket                                (domain.Wager)
--   legs                one selection on a ticket, holding the booked price
--                                                                   (domain.Leg)
--   ledger_transactions one balanced money movement        (domain.Transaction)
--   ledger_entries      one signed half of a movement      (domain.LedgerEntry)
--
--   account_balances    the derived balance of every account (a VIEW, never a
--                       table and never a materialised view -- see below)
--   ledger_integrity    domain.LedgerIsBalanced as a query: the audit hook
--
--
-- THERE IS NO BALANCE COLUMN. ANYWHERE. ON ANY TABLE.
-- --------------------------------------------------
-- CLAUDE.md §4: "Balances are derived, never stored as a mutable field."
-- internal/domain/ledger.go says the same thing about itself: "There is no
-- settable balance anywhere in the package, no cached total on Account, and no
-- incremental update path -- a balance is a pure fold over entries and nothing
-- else, so it cannot drift from the rows that produced it, which is the single
-- most common defect in hand-rolled ledgers."
--
-- 00005_accounts_and_auth.sql honours the same rule on its side: `users` carries
-- no balance column and says so in its header.
--
-- A balance is `SELECT sum(amount_minor) ... GROUP BY account`, exposed as the
-- `account_balances` view below.
--
-- WHY THE VIEW IS NOT MATERIALISED, even though ledger.go says a materialised
-- view is the answer to the O(n) fold: a materialised view is a STORED COPY of a
-- balance. Between two REFRESHes it is wrong, and "the balance was right an hour
-- ago" is precisely the defect the derived rule exists to prevent -- a bet slip
-- validated against a stale balance is an overdraft. On the phase-10 deploy
-- target (2 OCPU / 12 GB Ampere, see the ledger contract) REFRESH MATERIALIZED
-- VIEW CONCURRENTLY on every ledger write costs more than the fold it replaces,
-- because `ledger_entries_account_idx` makes the fold an index-only scan over
-- one account's rows. If it ever does become the bottleneck the fix is a
-- rollup-by-day projection that can be dropped and rebuilt from these same
-- entries -- still derived, never a mutable field. That is a separate, reviewed
-- migration with a measurement attached, not a default.
--
--
-- ENUM REPRESENTATION: TEXT + CHECK -- MATCHED, NOT CHOSEN
-- --------------------------------------------------------
-- 00005_accounts_and_auth.sql and 00007_platform.sql were already on disk and
-- had already made this decision, at length, under the headings "ENUM
-- REPRESENTATION: TEXT + CHECK, EVERYWHERE" and "ENUM REPRESENTATION: TEXT +
-- CHECK, NOT NATIVE ENUM TYPES". 00001_extensions_and_enums.sql independently
-- confirms it and carries the authoritative value catalogue. This file matches
-- them: TEXT constrained by a named CHECK, never `CREATE TYPE ... AS ENUM`,
-- never a DOMAIN, never a lookup table.
--
-- Their deciding argument is CLAUDE.md §12 reversibility -- a CHECK has an exact
-- one-line inverse and a native ENUM has no DROP VALUE -- and it applies here
-- unchanged. Consistency across the schema outranks any preference of mine.
--
-- The six value sets this file constrains, each read out of the `String()` switch
-- body that emits it (not inferred from the constant identifier):
--
--   wagers.kind          domain.WagerKind.String()    wager.go:217
--                        straight | parlay | round_robin | teaser
--   wagers.status        domain.WagerStatus.String()  wager.go:357
--                        placed | open | won | lost | void | push | cashed_out
--   wagers.rounding      domain.Rounding.String()     money.go:92
--                        half_away_from_zero | half_to_even | toward_zero
--   legs.status          domain.LegStatus.String()    leg.go:104
--                        pending | won | lost | void | push
--   legs.market_type     domain.MarketType.String()   market.go:201
--   legs.role            domain.SelectionRole.String() selection.go:45
--   ledger_*.kind        domain.EntryKind.String()    ledger.go:337
--                        grant | stake | payout | loss | refund | cash_out
--                        | adjustment
--   ledger_entries.account_kind
--                        domain.AccountKind.String()  ledger.go:131
--                        user_cash | user_escrow | house | issuance
--
-- No CHECK list contains 'unknown'. Every one of these Go types has an invalid
-- zero value whose MarshalText returns an error, so "unknown" is not a storable
-- value in this system; a column meaning "not yet known" is NULL.
--
--
-- ONE DISAGREEMENT WITH 00001'S CATALOGUE, RECORDED RATHER THAN RESOLVED QUIETLY
-- -----------------------------------------------------------------------------
-- 00001 lists `Rounding` under "ENUMS IN GO THAT MUST NOT BECOME COLUMNS", on
-- the grounds that it is "A PARAMETER TO A CALCULATION. Nothing is 'a row with a
-- rounding mode'."
--
-- `wagers.rounding` exists anyway, because domain.Wager disagrees: it stores the
-- rounding mode as a field and exposes it, with a reason stated in wager.go:
--
--     "Rounding returns the rule stake x price was collapsed under, so a later
--      recomputation -- a partial void repricing a parlay -- uses the same rule
--      the ticket was written under rather than picking a fresh one."
--
-- Three consequences make the column load-bearing rather than decorative.
-- NewWager REQUIRES a valid Rounding and refuses the zero value, so a wager
-- rehydrated from these rows without it cannot be constructed at all. Settlement
-- of a partially-voided parlay must reprice under the ticket's original rule, and
-- money.go is explicit that "a silent default is how a house edge appears in a
-- ledger that nobody meant to put one in". And a rounding mode chosen fresh at
-- settlement would make the stored potential_payout_minor unreproducible.
--
-- So a wager IS "a row with a rounding mode", and 00001's catalogue is right
-- about the type in general and wrong about this one column. Flagged rather than
-- edited: 00001 is not mine to change.
--
--
-- MONEY IS BIGINT MINOR UNITS. NO EXCEPTIONS ON THIS PAGE.
-- --------------------------------------------------------
-- CLAUDE.md §12: "All money and stake values are integer minor units. Floating
-- point never touches a balance. Odds and probabilities are floats; ledger
-- amounts are not."
--
-- Every money column here is BIGINT and every one is named `*_minor` so the unit
-- is impossible to mistake at a call site. A NUMERIC or DOUBLE PRECISION money
-- column in this schema is a defect, and in this table specifically it would be a
-- correctness defect and not a style one: ledger.go's zero-sum test is EXACT
-- integer equality --
--
--     "This is the one place in the codebase where exact equality is the right
--      test rather than a mistake -- and it is exactly why CLAUDE.md §12 puts
--      money in integers in the first place. A float ledger would need a
--      tolerance, and a ledger with a tolerance is a ledger that can lose a cent
--      per row."
--
-- and the balance trigger below is that test, in SQL, on the same integers.
--
-- The bound 9007199254740991 recurring in the CHECKs is domain.MaxSafeMoney
-- (2^53-1): the largest integer float64 holds exactly, and JavaScript's
-- Number.MAX_SAFE_INTEGER, so a value survives the trip to the Next.js frontend
-- as a JSON number. 00005 uses the same literal for the same reason.
--
-- Odds (`accepted_decimal`, `price_decimal`) and lines (`price_line`,
-- `teased_line`, `teaser_points`) are DOUBLE PRECISION, per the same sentence of
-- §12 read in the other direction.
--
--
-- WHAT IS DELIBERATELY DERIVED AND THEREFORE NOT A COLUMN
-- -------------------------------------------------------
--   * any account balance                 -- see above; `account_balances` view
--   * wagers.settled_at                   -- Wager.SettledAt() is UpdatedAt()
--                                            once terminal. wager.go: "a separate
--                                            field would be a second copy of the
--                                            same instant and therefore a second
--                                            thing to keep in agreement."
--   * round_robins.total_stake_minor      -- RoundRobin.TotalStake() is
--                                            stake_per_combination x ticket count
--   * round_robins.combination_count      -- sum of C(n,k) over its sizes
--   * wagers.leg_count                    -- count(*) over legs
--   * legs.grading_line                   -- Leg.GradingLine() is COALESCE(
--                                            teased_line, price_line)
--   * any per-user exposure / "at risk"   -- sum over user_escrow entries
--   * wager P&L, ROI, CLV                 -- folds for phase 9, computed from
--                                            legs.price_* and wagers.returned_*
--
--
-- NO SEED DATA. NOT ONE ROW.
-- --------------------------
-- The value sets above are CHECK constraints, so there is nothing to seed by
-- construction: no wager_kinds table, no account_kinds table, no chart of
-- accounts. domain.Account is deliberately identified by its (kind, owner) pair
-- rather than by a row -- ledger.go: "An account IS its (kind, owner) pair" --
-- so there is no `accounts` table to populate either, and the four system-wide
-- account kinds spring into existence the moment an entry names one.
--
-- An empty wagers / ledger after `make up` is CORRECT. Money enters through
-- EntryKindGrant, written by the application, audited in audit_log (00007).
--
--
-- HARD DEPENDENCIES ON MIGRATIONS OWNED BY OTHER AGENTS
-- -----------------------------------------------------
--   users      (id TEXT PRIMARY KEY)          00005_accounts_and_auth.sql
--   events     (id TEXT PRIMARY KEY)          catalogue migration
--   markets    (id, type) UNIQUE              catalogue migration
--   selections (id TEXT PRIMARY KEY)          catalogue migration
--   books      (id TEXT PRIMARY KEY)          catalogue migration
--
-- All five are asserted by the preflight below, so a missing prerequisite fails
-- with a sentence a human can act on rather than with a bare relation-does-not-
-- exist deep inside a CREATE TABLE.
--
-- NOTE WHAT IS *NOT* IN THAT LIST: `prices`. This migration has no dependency on
-- the price hypertable, deliberately and permanently -- see the header on `legs`.
--
-- +goose Up

-- ---------------------------------------------------------------------------
-- Preflight. CLAUDE.md §12's "fail fast and loudly on a bad config", applied to
-- schema prerequisites. Mirrors the pattern 00007_platform.sql established.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $preflight$
DECLARE
    missing TEXT[] := ARRAY[]::TEXT[];
BEGIN
    IF to_regclass('public.users') IS NULL THEN
        missing := missing || 'users (00005_accounts_and_auth.sql)';
    END IF;
    IF to_regclass('public.events') IS NULL THEN
        missing := missing || 'events (catalogue migration)';
    END IF;
    IF to_regclass('public.markets') IS NULL THEN
        missing := missing || 'markets (catalogue migration)';
    END IF;
    IF to_regclass('public.selections') IS NULL THEN
        missing := missing || 'selections (catalogue migration)';
    END IF;
    IF to_regclass('public.books') IS NULL THEN
        missing := missing || 'books (catalogue migration)';
    END IF;

    IF array_length(missing, 1) IS NOT NULL THEN
        RAISE EXCEPTION
            'migration 00006_wagers_and_ledger requires table(s) that do not exist: %',
            array_to_string(missing, ', ')
            USING HINT = 'Wagers reference users, events, markets, selections and books. '
                         'Every one of those migrations must be numbered lower than 00006.';
    END IF;

    -- The composite foreign key on `legs` targets markets (id, type), which
    -- requires that pair to carry a UNIQUE or PRIMARY KEY constraint. The
    -- catalogue migration declares it as `markets_id_type_key`. Assert it by
    -- capability rather than by name, so a rename upstream is not a false alarm.
    IF NOT EXISTS (
        SELECT 1
          FROM pg_constraint c
          JOIN pg_class     t ON t.oid = c.conrelid
         WHERE t.relname  = 'markets'
           AND c.contype IN ('p', 'u')
           -- attname is `name`, not `text`; the cast is required or the comparison
           -- fails with "operator does not exist: name[] = text[]".
           AND (SELECT array_agg(a.attname::TEXT ORDER BY a.attname::TEXT)
                  FROM unnest(c.conkey) AS k(attnum)
                  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum)
               = ARRAY['id', 'type']::TEXT[]
    ) THEN
        RAISE EXCEPTION
            'migration 00006_wagers_and_ledger requires a UNIQUE or PRIMARY KEY on markets (id, type)'
            USING HINT = 'legs carries a denormalised market_type pinned to its market by a '
                         'composite foreign key, exactly as selections does. The catalogue '
                         'migration declares this as markets_id_type_key.';
    END IF;
END
$preflight$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- Shared trigger function: maintain updated_at on the two MUTABLE tables here.
--
-- Namespaced `betting_` for the reason 00007 spells out about
-- `platform_set_updated_at()`: a bare `set_updated_at()` is the name every
-- concurrently-authored migration reaches for, two of them collide on the way
-- up, and the first Down to run drops a function the other's triggers still
-- need. 00002 namespaced its own as `catalogue_set_updated_at()`. This file
-- neither creates nor drops either of theirs.
--
-- ON THE 00005-vs-00002 DISAGREEMENT ABOUT updated_at TRIGGERS, and which side
-- this file takes and why:
--
--   00005 refuses triggers outright -- "The domain's state transitions take the
--   instant as an explicit parameter (Wager.Settle(status, amount, at),
--   Leg.WithStatus(status, at)) precisely so that a redelivered Kafka message
--   re-applies the ORIGINAL instant rather than the wall clock. A trigger
--   overwriting that with now() would silently discard the value the domain
--   worked to preserve."
--
--   00002 splits the two meanings into two columns and triggers only the
--   bookkeeping one.
--
-- 00005's objection is exactly right, and it is answered rather than ignored:
-- the domain instant lives in its OWN column on every table here
-- (`wagers.transitioned_at`, `legs.graded_at`, `ledger_transactions.occurred_at`,
-- `legs.price_observed_at`) and no trigger ever touches those. `updated_at` is
-- row bookkeeping and nothing else. That is 00002's resolution, and it is
-- adopted here for consistency with the sibling migration that models the same
-- provider-instant-versus-row-instant problem.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION betting_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $betting_set_updated_at$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$betting_set_updated_at$;
-- +goose StatementEnd

COMMENT ON FUNCTION betting_set_updated_at() IS
    'BEFORE UPDATE trigger: stamps updated_at from the server clock. Never touches a domain instant (transitioned_at, graded_at, occurred_at, price_observed_at). Namespaced to this migration so its Down cannot orphan another migration''s triggers.';

-- ---------------------------------------------------------------------------
-- round_robins
--
-- domain.RoundRobin. CLAUDE.md §6 lists round robin among the wager kinds, and
-- wager.go states the relationship this table exists to hold:
--
--     "A '3-team round robin by 2s' is not one bet: it is three independent
--      two-leg parlays -- AB, AC, BC -- each of which wins, loses, and settles
--      on its own. Modelling it as one wager would make 'how much did ticket AC
--      return' unanswerable."
--
-- So the expansion is one parent row here and N ticket rows in `wagers`, each
-- naming this parent. `wagers` enforces the biconditional the domain enforces
-- (validateRoundRobinParent): kind = 'round_robin' EXACTLY WHEN round_robin_id
-- is present.
--
-- WHY THE PARENT'S OWN LEG SET IS NOT STORED HERE
-- ------------------------------------------------
-- domain.RoundRobin carries `legs` -- the selections the combinations are drawn
-- from -- and this table does not. Every one of those selections appears on at
-- least one expanded ticket (for any 2 <= k <= n, every index is in some
-- k-subset), so the parent's selection set is exactly
--
--     SELECT DISTINCT selection_id FROM legs JOIN wagers ... WHERE round_robin_id = $1
--
-- A second stored copy would be independently writable and could therefore
-- disagree with the tickets it supposedly generated -- and the copy, not the
-- tickets, is the one nobody would notice was wrong.
--
-- `selection_count` IS stored, and is not a second copy of that set: it is the
-- `n` in C(n, k), the bound every combination size is validated against, and
-- domain.MaxRoundRobinLegs's ceiling. wager.go on why that ceiling matters:
-- "the ticket count is a binomial coefficient, so this number sits inside an
-- exponential. At 10 selections ... every size together is 2^10 - 11 = 1013. At
-- 20 it would be a million, which is not a large bet, it is a denial of service
-- against the settlement path."
-- ---------------------------------------------------------------------------
CREATE TABLE round_robins (
    id                          TEXT        PRIMARY KEY
                                            CONSTRAINT round_robins_id_charset
                                            CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- ON DELETE RESTRICT, matching 00005's rule for every reference to users
    -- except the TOTP row: "auth history and money movements must survive."
    user_id                     TEXT        NOT NULL
                                            REFERENCES users (id) ON DELETE RESTRICT
                                            CONSTRAINT round_robins_user_id_charset
                                            CHECK (user_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- len(RoundRobin.Legs()). NewRoundRobin refuses fewer than 2 and more than
    -- domain.MaxRoundRobinLegs (10).
    selection_count             INTEGER     NOT NULL
                                            CONSTRAINT round_robins_selection_count_range
                                            CHECK (selection_count BETWEEN 2 AND 10),

    -- RoundRobin.StakePerCombination(): the stake on EACH generated ticket, not
    -- the total. wager.go: "That is how books quote it and how customers think
    -- about it: '$5 round robin by 2s' on four selections risks $30, not $5."
    stake_per_combination_minor BIGINT      NOT NULL
                                            CONSTRAINT round_robins_stake_range
                                            CHECK (stake_per_combination_minor > 0
                                                   AND stake_per_combination_minor <= 9007199254740991),

    -- RoundRobin.PlacedAt(), in UTC. A domain instant: no trigger touches it.
    placed_at                   TIMESTAMPTZ NOT NULL
                                            CONSTRAINT round_robins_placed_at_sane
                                            CHECK (placed_at > '1900-01-01T00:00:00Z'),

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Target for round_robin_sizes' composite FK, which is what lets
    -- "2 <= size <= selection_count" be a CHECK instead of a trigger.
    CONSTRAINT round_robins_id_selection_count_key UNIQUE (id, selection_count),

    -- Target for the composite FK on wagers, which is what makes "every ticket
    -- of a round robin carries the round robin's per-ticket stake" unstorable
    -- when violated. RoundRobin.TotalStake() depends on that identity: "the
    -- total can never disagree with the sum of the individual tickets' stakes by
    -- a minor unit, which is the property the ledger's balance check depends on."
    CONSTRAINT round_robins_id_stake_key UNIQUE (id, stake_per_combination_minor)
);

-- No updated_at, and no updated_at trigger: domain.RoundRobin is immutable. It
-- has no With* method, no state machine, and no transition -- once placed, an
-- expansion is a historical fact. The frozen convention asks for updated_at on
-- MUTABLE tables; this is not one.
--
-- Its columns are also immutable in practice without a trigger, which is worth
-- noting because it is emergent rather than declared: both UNIQUE pairs above
-- are foreign-key targets, and an FK's default ON UPDATE NO ACTION means
-- `selection_count` and `stake_per_combination_minor` cannot be changed while
-- any child row references the old value.

COMMENT ON TABLE round_robins IS
    'One placed round robin (domain.RoundRobin). Expands into N independent tickets in `wagers`, each naming this row as its parent. The parent''s own selection set is derived from those tickets, never stored twice.';
COMMENT ON COLUMN round_robins.selection_count IS
    'len(RoundRobin.Legs()) -- the n in C(n,k). Bounded 2..10 by domain.MaxRoundRobinLegs, which caps an exponential ticket count.';
COMMENT ON COLUMN round_robins.stake_per_combination_minor IS
    'BIGINT minor units. The stake on EACH generated ticket, not the total; RoundRobin.TotalStake() is this times the ticket count and is deliberately not stored.';

-- ---------------------------------------------------------------------------
-- round_robin_sizes
--
-- RoundRobinParams.Sizes: "{2} for 'by 2s', {2, 3} for 'by 2s and 3s'. Each must
-- be at least 2 and at most len(Legs). The slice is sorted and de-duplicated, so
-- {3, 2, 3} and {2, 3} describe the same round robin."
--
-- A child table rather than an INTEGER[] column on the parent, because every one
-- of those three rules becomes declarative here and none of them can be
-- expressed against an array:
--
--   sorted + de-duplicated  -> the PRIMARY KEY (round_robin_id, size) makes a
--                              duplicate unstorable, and ORDER BY size
--                              reproduces the domain's canonical order exactly.
--                              An array CHECK cannot test sortedness or
--                              distinctness without a subquery, which CHECK
--                              forbids.
--   2 <= size               -> a plain CHECK.
--   size <= len(legs)       -> a CHECK against the denormalised selection_count,
--                              pinned to the parent by the composite FK below.
--                              This is the same device the catalogue migration
--                              uses for selections.market_type, for the same
--                              reason: a CHECK cannot read another table, and a
--                              copy that the database refuses to let disagree is
--                              better than a trigger.
--
-- ON DELETE CASCADE, chosen rather than defaulted, and the only CASCADE in this
-- file. These rows ARE the parent's definition -- they have no meaning apart
-- from it and nothing hangs below them -- which is the test 00007 applied to
-- market_suspensions. Every other FK here is RESTRICT because everything else on
-- this page is money or evidence.
-- ---------------------------------------------------------------------------
CREATE TABLE round_robin_sizes (
    round_robin_id  TEXT    NOT NULL,

    -- Denormalised copy of round_robins.selection_count, held honest by the
    -- composite FK. Never written independently: it is whatever the parent says,
    -- and the database refuses any other value.
    selection_count INTEGER NOT NULL,

    size            INTEGER NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT round_robin_sizes_pkey PRIMARY KEY (round_robin_id, size),

    CONSTRAINT round_robin_sizes_parent_fk
        FOREIGN KEY (round_robin_id, selection_count)
        REFERENCES round_robins (id, selection_count)
        ON DELETE CASCADE,

    -- NewRoundRobin: "if k < 2 || k > len(legs)" is an error.
    CONSTRAINT round_robin_sizes_size_range
        CHECK (size BETWEEN 2 AND selection_count)
);

COMMENT ON TABLE round_robin_sizes IS
    'The combination sizes a round robin expands by (RoundRobinParams.Sizes). A child table rather than an array so that sortedness, distinctness, and "size <= selection_count" are all declarative.';
COMMENT ON COLUMN round_robin_sizes.selection_count IS
    'Denormalised copy of round_robins.selection_count, pinned by the composite FK. Exists solely so "size <= len(legs)" can be a CHECK rather than a trigger.';

-- ---------------------------------------------------------------------------
-- wagers
--
-- domain.Wager. One ticket: straight, parlay, one combination of a round robin,
-- or a teaser.
--
-- THE TICKET PRICE AND THE PAYOUT ARE STORED, NOT DERIVED
-- -------------------------------------------------------
-- wager.go, at length, because this is the decision the table is shaped around:
--
--     "A parlay's price is not always the product of its legs. Same-game legs
--      are correlated and are priced with a correlation adjustment (CLAUDE.md
--      §4), a teaser's price is a fixed schedule that has nothing to do with the
--      underlying prices at all, and a boosted ticket is priced above both.
--      Re-deriving the price at settlement would therefore produce a different
--      number than the customer was shown and accepted.
--
--      So the accepted price is recorded, and the potential payout is computed
--      once, at placement, under an explicit Rounding, and frozen. 'To win $X'
--      is a promise, and a promise recomputed later is not one."
--
-- Hence `accepted_decimal`, `rounding`, `potential_payout_minor` and
-- `potential_profit_minor` are all columns, and none of them is recomputed by
-- anything. The `wagers_assert_transition` trigger below makes them immutable
-- after insert, so "frozen" is a property of the database and not a convention.
--
-- WHAT IS DELIBERATELY *NOT* CONSTRAINED: THE ROUNDING IDENTITY
-- -------------------------------------------------------------
-- NewWager computes payout = Stake.MulFloat(AcceptedDecimal, Rounding), so in
-- principle the schema could assert
--
--     potential_payout_minor = round(stake_minor * accepted_decimal)
--
-- It deliberately does not. Money.MulFloat implements three distinct rounding
-- modes -- half-away-from-zero, half-to-even, toward-zero -- with overflow
-- guards, and PostgreSQL's round() offers only the first. Reproducing the other
-- two in SQL would be a SECOND IMPLEMENTATION of the domain's money math, which
-- is the thing CLAUDE.md §10 is bluntest about: "two implementations of one
-- formula eventually disagree". A disagreement here does not merely report a
-- wrong number, it REJECTS a legitimate wager at placement.
--
-- What is enforced instead are the two identities that hold exactly under every
-- rounding mode, and that NewWager itself checks:
--
--   potential_payout_minor >= stake_minor       (ErrPayoutBelowStake:
--                                                "Winning must never cost money")
--   potential_profit_minor  = payout - stake    (exact integer subtraction)
--
-- Reconciling the stored payout against a recomputation is a job for the Go test
-- suite and for phase 9's reconciliation queries, where a mismatch is a report
-- rather than a rejected bet.
-- ---------------------------------------------------------------------------
CREATE TABLE wagers (
    id                     TEXT        PRIMARY KEY
                                       CONSTRAINT wagers_id_charset
                                       CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.UserID. RESTRICT: a placed bet is a money record and must outlive
    -- any attempt to delete the account that placed it.
    user_id                TEXT        NOT NULL
                                       REFERENCES users (id) ON DELETE RESTRICT
                                       CONSTRAINT wagers_user_id_charset
                                       CHECK (user_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.WagerKind.String(). Exactly the four CLAUDE.md §6 names.
    kind                   TEXT        NOT NULL
                                       CONSTRAINT wagers_kind_defined
                                       CHECK (kind IN ('straight', 'parlay',
                                                       'round_robin', 'teaser')),

    -- domain.WagerStatus.String(). The lifecycle, whose legal edges are enforced
    -- by wagers_assert_transition below rather than by this list alone.
    -- No DEFAULT: the catalogue migration gives events.status and markets.status
    -- none either, and a default that silently supplies a lifecycle state is how
    -- a mis-mapped INSERT produces a plausible row.
    status                 TEXT        NOT NULL
                                       CONSTRAINT wagers_status_defined
                                       CHECK (status IN ('placed', 'open', 'won', 'lost',
                                                         'void', 'push', 'cashed_out')),

    -- Wager.Stake(). BIGINT minor units, strictly positive (ErrStakeNotPositive).
    stake_minor            BIGINT      NOT NULL
                                       CONSTRAINT wagers_stake_range
                                       CHECK (stake_minor > 0
                                              AND stake_minor <= 9007199254740991),

    -- Wager.AcceptedDecimal(): total return per unit staked with every leg
    -- winning. Range is (domain.MinDecimalOdds, domain.MaxWagerDecimal] =
    -- (1.0, 1e9] -- deliberately NOT MaxDecimalOdds, which bounds a single quoted
    -- market price. wager.go: "A 20-leg parlay of even-money legs is 2^20 = 1.05e6
    -- in decimal odds, which is a perfectly ordinary ticket and which
    -- MaxDecimalOdds (1e5) would wrongly reject."
    --
    -- The upper bound also rejects NaN and +Infinity without a separate test:
    -- PostgreSQL orders NaN above every float8, so `accepted_decimal <= 1e9` is
    -- false for both.
    accepted_decimal       DOUBLE PRECISION NOT NULL
                                       CONSTRAINT wagers_accepted_decimal_range
                                       CHECK (accepted_decimal > 1.0
                                              AND accepted_decimal <= 1000000000.0),

    -- domain.Rounding.String(). See the header note on 00001's catalogue: this
    -- column exists because Wager stores the mode and NewWager refuses the
    -- invalid zero value, so a wager cannot be rehydrated without it. No DEFAULT,
    -- for the reason money.go gives: "There is no default. The zero value is
    -- invalid and every float-consuming operation rejects it, which forces the
    -- rounding decision to be written at the call site where it can be reviewed."
    rounding               TEXT        NOT NULL
                                       CONSTRAINT wagers_rounding_defined
                                       CHECK (rounding IN ('half_away_from_zero',
                                                           'half_to_even',
                                                           'toward_zero')),

    -- Wager.PotentialPayout(): TOTAL RETURN if every leg wins, stake included.
    potential_payout_minor BIGINT      NOT NULL
                                       CONSTRAINT wagers_potential_payout_range
                                       CHECK (potential_payout_minor > 0
                                              AND potential_payout_minor <= 9007199254740991),

    -- Wager.PotentialProfit(): NET WINNINGS, payout minus stake. Stored rather
    -- than derived in SQL for the reason wager.go names the pair as bluntly as it
    -- does -- "conflating return with profit produces a plausible number of the
    -- right magnitude" -- and pinned to the identity by a CHECK below, so the two
    -- columns cannot disagree.
    potential_profit_minor BIGINT      NOT NULL
                                       CONSTRAINT wagers_potential_profit_range
                                       CHECK (potential_profit_minor >= 0
                                              AND potential_profit_minor <= 9007199254740991),

    -- Wager.TeaserPoints(): the points EVERY leg's line is moved by. Present
    -- exactly on a teaser (validateTeaser), and bounded (0, domain.
    -- MaxTeaserPoints=20]. The upper bound rejects NaN and +Infinity; `> 0`
    -- rejects -Infinity.
    teaser_points          DOUBLE PRECISION
                                       CONSTRAINT wagers_teaser_points_range
                                       CHECK (teaser_points IS NULL
                                              OR (teaser_points > 0
                                                  AND teaser_points <= 20.0)),

    -- domain.RoundRobinID. Present exactly on a round-robin ticket
    -- (validateRoundRobinParent). There is no separate FK to round_robins (id):
    -- the composite constraint below subsumes it, since the pair must exist in
    -- the parent, which implies the id does. The catalogue migration makes the
    -- same argument about selections' pair FK.
    round_robin_id         TEXT
                                       CONSTRAINT wagers_round_robin_id_charset
                                       CHECK (round_robin_id IS NULL
                                              OR round_robin_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Wager.Returned(): what settlement actually paid back. wager.go: it "is the
    -- ONLY authority on what settlement owes: a partially-voided parlay returns
    -- less than Wager.PotentialPayout(), and a cash-out returns whatever price
    -- was taken." NULL exactly while the ticket is still running.
    returned_minor         BIGINT
                                       CONSTRAINT wagers_returned_range
                                       CHECK (returned_minor IS NULL
                                              OR (returned_minor >= 0
                                                  AND returned_minor <= 9007199254740991)),

    -- Wager.NetReturn(): returned minus stake, NEGATIVE on a loser. The per-wager
    -- P&L the leaderboard's ROI is built from (CLAUDE.md §6).
    net_return_minor       BIGINT
                                       CONSTRAINT wagers_net_return_range
                                       CHECK (net_return_minor IS NULL
                                              OR (net_return_minor >= -9007199254740991
                                                  AND net_return_minor <= 9007199254740991)),

    -- Wager.PlacedAt(), UTC. A domain instant; no trigger touches it.
    placed_at              TIMESTAMPTZ NOT NULL
                                       CONSTRAINT wagers_placed_at_sane
                                       CHECK (placed_at > '1900-01-01T00:00:00Z'),

    -- Wager.UpdatedAt(): the instant of the most recent transition, from the
    -- SERVICE's clock, passed explicitly into Open/GradeLeg/Settle/CashOut.
    --
    -- Its own column, separate from `updated_at`, for the reason the catalogue
    -- migration split observed_at from updated_at: collapsing two meanings into
    -- one column guarantees that somebody eventually stamps the domain instant
    -- with now(). Wager.SettledAt() is this column once the status is terminal,
    -- which is why there is no settled_at column.
    transitioned_at        TIMESTAMPTZ NOT NULL,

    created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at             TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- validateTeaser's biconditional, exact rather than one-sided because
    -- NewWager rejects BOTH a teaser without points and a non-teaser with them
    -- (ErrTeaserPointsRequired / ErrTeaserPointsNotApplicable).
    CONSTRAINT wagers_teaser_points_matches_kind
        CHECK ((kind = 'teaser') = (teaser_points IS NOT NULL)),

    -- validateRoundRobinParent, verbatim: "a ticket is of kind round_robin
    -- exactly when it names a parent round robin".
    CONSTRAINT wagers_round_robin_matches_kind
        CHECK ((kind = 'round_robin') = (round_robin_id IS NOT NULL)),

    -- The payout/profit identity from NewWager, exactly.
    CONSTRAINT wagers_potential_payout_covers_stake
        CHECK (potential_payout_minor >= stake_minor),
    CONSTRAINT wagers_potential_profit_identity
        CHECK (potential_profit_minor = potential_payout_minor - stake_minor),

    -- Wager.Returned()/NetReturn() return their value together with the SAME
    -- `hasReturned` flag, and settleAt sets both at once. Two nulls or two
    -- values; never one of each.
    CONSTRAINT wagers_return_pair_complete
        CHECK ((returned_minor IS NULL) = (net_return_minor IS NULL)),
    CONSTRAINT wagers_net_return_identity
        CHECK (net_return_minor IS NULL
               OR net_return_minor = returned_minor - stake_minor),

    -- settleAt is the ONLY writer of returned/netReturn and it always applies a
    -- terminal status, so "has a returned amount" and "is terminal" are the same
    -- fact. WagerStatus.IsTerminal(), spelled out.
    CONSTRAINT wagers_return_iff_terminal
        CHECK ((status IN ('won', 'lost', 'void', 'push', 'cashed_out'))
               = (returned_minor IS NOT NULL)),

    -- Wager.checkReturn() and Wager.CashOut(), per outcome. These are integer
    -- comparisons on minor units, so they are exact -- wager.go: "The upper bound
    -- on a win is the one that matters: a partially-voided parlay legitimately
    -- pays LESS than the ticket's headline payout, and nothing legitimately pays
    -- more, so a return above the maximum is an arithmetic fault caught here
    -- rather than an overpayment discovered in a reconciliation."
    --
    -- Each terminal arm re-tests IS NOT NULL rather than leaning on
    -- wagers_return_iff_terminal, because a CHECK that evaluates to NULL PASSES:
    -- `returned_minor = 0` against a NULL is NULL, not false. ELSE FALSE refuses
    -- any status this CASE does not name, since a CASE with no matching arm also
    -- yields NULL.
    CONSTRAINT wagers_return_matches_outcome
        CHECK (CASE status
                   WHEN 'placed'     THEN returned_minor IS NULL
                   WHEN 'open'       THEN returned_minor IS NULL
                   WHEN 'lost'       THEN returned_minor IS NOT NULL
                                          AND returned_minor = 0
                   WHEN 'void'       THEN returned_minor IS NOT NULL
                                          AND returned_minor = stake_minor
                   WHEN 'push'       THEN returned_minor IS NOT NULL
                                          AND returned_minor = stake_minor
                   WHEN 'won'        THEN returned_minor IS NOT NULL
                                          AND returned_minor >= stake_minor
                                          AND returned_minor <= potential_payout_minor
                   WHEN 'cashed_out' THEN returned_minor IS NOT NULL
                                          AND returned_minor > 0
                                          AND returned_minor <= potential_payout_minor
                   ELSE FALSE
               END),

    -- Wager.stamp(): every transition instant is at or after the one before it,
    -- and the first one is placed_at. A row invariant, not merely a write rule.
    CONSTRAINT wagers_transitioned_after_placed
        CHECK (transitioned_at >= placed_at),

    -- Every ticket of a round robin carries the round robin's per-ticket stake.
    -- MATCH SIMPLE (the default) is exactly what is wanted: when round_robin_id
    -- is NULL the constraint is not checked at all, and when it is present
    -- stake_minor is NOT NULL, so the pair is always fully checked on precisely
    -- the rows the rule applies to.
    --
    -- ON UPDATE NO ACTION (the default) makes round_robins.stake_per_combination_
    -- minor immutable while any ticket references it, which is correct: the stake
    -- a customer was charged is not editable after the fact.
    CONSTRAINT wagers_round_robin_stake_fk
        FOREIGN KEY (round_robin_id, stake_minor)
        REFERENCES round_robins (id, stake_per_combination_minor)
        ON DELETE RESTRICT
);

CREATE TRIGGER wagers_set_updated_at
    BEFORE UPDATE ON wagers
    FOR EACH ROW EXECUTE FUNCTION betting_set_updated_at();

COMMENT ON TABLE wagers IS
    'One placed ticket (domain.Wager). The accepted price, rounding mode and potential payout are frozen at placement and made immutable by trigger; a settled wager is terminal and cannot be re-graded.';
COMMENT ON COLUMN wagers.accepted_decimal IS
    'Wager.AcceptedDecimal(): the ticket price the customer accepted. Stored, never derived from the legs -- a correlated same-game parlay and a teaser are both priced by rules the leg prices do not determine.';
COMMENT ON COLUMN wagers.rounding IS
    'domain.Rounding. Recorded so a later repricing (a partially-voided parlay) uses the rule the ticket was written under. No default: a silent default is how an unintended house edge appears in the ledger.';
COMMENT ON COLUMN wagers.returned_minor IS
    'BIGINT minor units actually returned at settlement -- the only authority on what settlement owes. NULL exactly while the ticket is still running.';
COMMENT ON COLUMN wagers.transitioned_at IS
    'Wager.UpdatedAt(): the instant of the latest transition, from the acting service''s clock. Wager.SettledAt() is this column once the status is terminal, which is why no settled_at column exists.';
COMMENT ON COLUMN wagers.updated_at IS
    'Row bookkeeping, stamped by trigger. NOT a domain instant -- see transitioned_at.';

-- ---------------------------------------------------------------------------
-- legs
--
-- THE PRICE AT PLACEMENT TIME. THIS IS THE TABLE THE CHARTER EMPHASISES.
-- ----------------------------------------------------------------------
-- CLAUDE.md §4, the charter's own emphasis: "Legs hold the price *at placement
-- time*, never a live reference."
--
-- READ THIS BEFORE "NORMALISING" THE PRICE COLUMNS BELOW.
--
-- `price_book_id`, `price_decimal`, `price_line` and `price_observed_at` are a
-- COPY of one domain.Price value. They look exactly like a foreign key waiting
-- to be extracted into a reference to the `prices` hypertable, and extracting
-- them would be a correctness defect -- the specific, invisible kind:
--
--   * A price row in the hypertable is one point in a MOVING series. Its natural
--     key is (selection_id, book_id, observed_at) and a new quote is a new row
--     (CLAUDE.md §4: "Immutable; a new price is a new row"). A foreign key into
--     that series can only be resolved by choosing a row -- and any rule for
--     choosing one ("the latest", "the one at placement") is a lookup that can
--     return a DIFFERENT number tomorrow than it returned at placement.
--
--   * leg.go states the guarantee this copy buys: "This type carries a Price
--     VALUE -- not a MarketID to look up, not a pointer into a live snapshot, not
--     an index into a cache -- so there is no expression anywhere in the program
--     that can re-resolve a booked leg to a moved line."
--
--   * And why a comment would not be enough: "the bug it prevents is invisible in
--     review: a leg that reads the current line grades correctly in every test
--     where the line never moves, and pays the wrong amount exactly when it
--     matters."
--
-- So this migration has NO foreign key into `prices`, has no dependency on that
-- table existing, and must never acquire one. `wagers_assert_transition`'s
-- sibling `legs_assert_transition` additionally makes every price column
-- immutable after insert, so the booked number cannot be edited either -- by the
-- application, by `make psql`, or by a future service. Denormalisation here is
-- the invariant, not a shortcut around a join.
--
-- What IS a foreign key: selection_id, market_id, event_id and price_book_id, all
-- ON DELETE RESTRICT. Those are the domain's relationship keys -- leg.go: "They
-- answer 'same game?' and 'same question?' and nothing else; no code may use them
-- to look up a price for a booked leg." A foreign key does not perform a lookup;
-- it guarantees the referenced row still exists, which is what phase 9's CLV join
-- (a leg's booked price against its market's CLOSING price) depends on. RESTRICT
-- rather than CASCADE because a cascade from a stale event would delete the
-- booked prices that ARE the CLV dataset; the catalogue migration chose RESTRICT
-- down its whole spine for the same reason.
--
-- WHY market_type AND role ARE COPIED HERE TOO
-- --------------------------------------------
-- leg.go: "Neither of them ever changes on a live market, so copying them is not
-- about drift -- it is about self-sufficiency. Grading a leg must not require
-- re-reading the Market it came from, because that read is precisely the thing
-- this type exists to make impossible. A leg carries everything the grader
-- needs."
--
-- The copy is pinned to its market by the composite FK (market_id, market_type)
-- -> markets (id, type), so it cannot be forged to disagree. Two differences from
-- the catalogue migration's identical device on `selections`, both deliberate:
--
--   ON UPDATE RESTRICT, not CASCADE. Cascading a market's type change into a
--   booked ticket would silently rewrite what a customer bet on -- the exact
--   class of edit §4 forbids. RESTRICT instead blocks the market change while any
--   bet references it, which is the correct answer: you cannot retroactively
--   change the terms of a placed bet.
--
--   The pair FK subsumes a plain market_id -> markets(id) reference, so there is
--   only one FK, as on selections.
--
-- LEG IDENTITY, AND A REQUIREMENT ON THE BETTING SERVICE (PHASE 8)
-- ----------------------------------------------------------------
-- `id` is a globally unique primary key carrying domain.LegID, like every other
-- domain identifier in this schema.
--
-- That has a consequence the round-robin expansion path MUST honour.
-- RoundRobin.Combinations() returns subsets of the SAME []Leg values, so leg
-- AB.a and leg AC.a arrive carrying one LegID. The betting service must MINT A
-- DISTINCT LegID PER (TICKET, SELECTION) when it turns those combinations into
-- wagers, or the second INSERT violates this primary key.
--
-- The alternative -- PRIMARY KEY (wager_id, id), letting one LegID repeat across
-- a round robin's tickets -- was rejected: each ticket's legs grade independently
-- and at different times (Leg.status and Leg.gradedAt are per-leg), so two rows
-- sharing an id would carry independent statuses, and an identifier that does not
-- identify is worse than a longer key.
-- ---------------------------------------------------------------------------
CREATE TABLE legs (
    id                TEXT        PRIMARY KEY
                                  CONSTRAINT legs_id_charset
                                  CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- RESTRICT: a leg holds the booked price, which is evidence. Deleting a
    -- ticket must not take its terms with it. (In practice a committed wager is
    -- undeletable anyway -- see the note below the table.)
    wager_id          TEXT        NOT NULL
                                  REFERENCES wagers (id) ON DELETE RESTRICT
                                  CONSTRAINT legs_wager_id_charset
                                  CHECK (wager_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Leg.EventID(). A relationship key: same-game detection (Wager.IsSameGame,
    -- which the correlation adjustment keys on) and the settle consumer's "which
    -- ungraded legs does this finished event touch".
    event_id          TEXT        NOT NULL
                                  REFERENCES events (id) ON DELETE RESTRICT
                                  CONSTRAINT legs_event_id_charset
                                  CHECK (event_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Leg.MarketID(). Answers "two legs on one market?" -- ErrDuplicateMarket,
    -- enforced by legs_wager_market_key below.
    market_id         TEXT        NOT NULL
                                  CONSTRAINT legs_market_id_charset
                                  CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Leg.MarketType(), copied at placement and pinned to the market by the
    -- composite FK below.
    market_type       TEXT        NOT NULL,

    -- Leg.SelectionID(). Also the selection that Leg.Price() quotes: NewLeg
    -- refuses a leg whose "price quotes a different selection"
    -- (ErrLegPriceMismatch), so ONE column serves both and the equality is
    -- structural rather than checked. A second price_selection_id column would be
    -- a second copy of one fact with a constraint to keep them equal.
    selection_id      TEXT        NOT NULL
                                  REFERENCES selections (id) ON DELETE RESTRICT
                                  CONSTRAINT legs_selection_id_charset
                                  CHECK (selection_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Leg.Role(), copied at placement.
    role              TEXT        NOT NULL,

    -- Price.BookID(): which book quoted the number this leg was booked at. For an
    -- in-house ticket that is the synthetic book (domain.NewSyntheticBook).
    price_book_id     TEXT        NOT NULL
                                  REFERENCES books (id) ON DELETE RESTRICT
                                  CONSTRAINT legs_price_book_id_charset
                                  CHECK (price_book_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Price.Decimal(): total return per unit staked. Range is
    -- (domain.MinDecimalOdds, domain.MaxDecimalOdds] = (1.0, 1e5] -- a SINGLE
    -- QUOTED MARKET PRICE, so MaxDecimalOdds and not the wager's MaxWagerDecimal.
    -- The two bounds differ on purpose and the difference is documented on
    -- wagers.accepted_decimal.
    --
    -- Decimal and not American or fractional: price.go says decimal "is the only
    -- format that is total over the useful range", and the phase-1 contract adds
    -- that American and Fractional are DISPLAY formats to be converted at the
    -- edge and discarded. No column in this schema is denominated in either.
    price_decimal     DOUBLE PRECISION NOT NULL
                                  CONSTRAINT legs_price_decimal_range
                                  CHECK (price_decimal > 1.0
                                         AND price_decimal <= 100000.0),

    -- Price.Line(): the line the quote was made at, FROM THIS SELECTION'S OWN
    -- PERSPECTIVE -- domain.EffectiveLine's value, already inverted for an away
    -- spread. NULL is domain.NoLine(); 0.0 is a stored pick'em, which is a real
    -- traded value. price.go on why the line rides on the price at all:
    --
    --     "'-3.5 at 1.91' followed by '-4 at 1.95' is a line move, while the same
    --      two prices with the line stripped look like a pure odds move, and CLV
    --      computed against a closing price at a different line is not CLV."
    --
    -- The finiteness test is written as an ordering comparison, matching the
    -- catalogue migration: PostgreSQL defines NaN as EQUAL to itself and GREATER
    -- than every other float8, so `price_line = price_line` would not catch NaN
    -- while this does.
    price_line        DOUBLE PRECISION
                                  CONSTRAINT legs_price_line_finite
                                  CHECK (price_line IS NULL
                                         OR (price_line > '-Infinity'::double precision
                                             AND price_line < 'Infinity'::double precision)),

    -- Price.ObservedAt(): when the QUOTE was seen (not when the bet was placed --
    -- that is wagers.placed_at). Keeping it makes a booked leg self-describing
    -- enough to answer "how stale was the price this customer took", which is the
    -- headline SLO's question (CLAUDE.md §9) asked about a settled ticket.
    price_observed_at TIMESTAMPTZ NOT NULL
                                  CONSTRAINT legs_price_observed_at_sane
                                  CHECK (price_observed_at > '1900-01-01T00:00:00Z'),

    -- Leg.TeasedLine(): the moved line a teaser leg actually grades at.
    --
    -- leg.go on why the leg keeps BOTH lines rather than forging a Price at the
    -- teased number: "Rather than forge a Price at the moved line -- which would
    -- corrupt the line history and destroy CLV, since the book never traded there
    -- -- the leg keeps the REAL market price it was booked against and carries the
    -- teased line beside it."
    --
    -- Deliberately NOT constrained to be positive on a total, unlike
    -- markets.line: a heavily teased low total may legitimately cross zero, and
    -- validateTeaser imposes no such rule.
    teased_line       DOUBLE PRECISION
                                  CONSTRAINT legs_teased_line_finite
                                  CHECK (teased_line IS NULL
                                         OR (teased_line > '-Infinity'::double precision
                                             AND teased_line < 'Infinity'::double precision)),

    -- domain.LegStatus.String(). Legal edges are enforced by
    -- legs_assert_transition; this list is the value set.
    status            TEXT        NOT NULL
                                  CONSTRAINT legs_status_defined
                                  CHECK (status IN ('pending', 'won', 'lost',
                                                    'void', 'push')),

    -- Leg.GradedAt(). Per leg, not per wager, because "the legs of a parlay grade
    -- at different times because they are on different games". A domain instant;
    -- no trigger touches it.
    graded_at         TIMESTAMPTZ
                                  CONSTRAINT legs_graded_at_sane
                                  CHECK (graded_at IS NULL
                                         OR graded_at > '1900-01-01T00:00:00Z'),

    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Leg.WithStatus sets gradedAt exactly when the next status is terminal, and
    -- LegStatus.IsTerminal() is every status but 'pending'. So the biconditional
    -- is exact.
    CONSTRAINT legs_graded_at_iff_graded
        CHECK ((status <> 'pending') = (graded_at IS NOT NULL)),

    -- domain.MarketType.AllowsRole(), verbatim -- the same matrix the catalogue
    -- migration enforces on `selections`, restated here because a leg is
    -- deliberately self-sufficient and must be interpretable without reading the
    -- selection it was booked from. NewLeg checks it (ErrRoleNotApplicable) and
    -- so does the database.
    --
    -- ELSE FALSE is load-bearing: a CASE with no matching arm yields NULL, and a
    -- CHECK that evaluates to NULL PASSES.
    CONSTRAINT legs_role_allowed
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN role IN ('home', 'away', 'draw')
                   WHEN 'spread'      THEN role IN ('home', 'away')
                   WHEN 'total'       THEN role IN ('over', 'under')
                   WHEN 'player_prop' THEN role IN ('over', 'under', 'outright')
                   WHEN 'futures'     THEN role = 'outright'
                   ELSE FALSE
               END),

    -- domain.MarketType.LineRule() applied to the BOOKED price's line, which is
    -- EffectiveLine(market, selection) and therefore present exactly when the
    -- market type says a line is. Identical in shape to the catalogue migration's
    -- markets_line_rule, including validateLine's extra rule that a total is
    -- strictly positive ("a threshold on combined scoring, which is non-negative
    -- by construction"). A spread may be 0.0 -- a pick'em -- and negative, and an
    -- away spread's line is the home line inverted.
    CONSTRAINT legs_price_line_rule
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN price_line IS NULL
                   WHEN 'futures'     THEN price_line IS NULL
                   WHEN 'spread'      THEN price_line IS NOT NULL
                   WHEN 'total'       THEN price_line IS NOT NULL AND price_line > 0
                   WHEN 'player_prop' THEN TRUE
                   ELSE FALSE
               END),

    -- Leg.WithTeasedLine refuses any market type with no line to move: "you
    -- cannot tease a moneyline, and a book that let you would be giving away the
    -- whole edge the teaser price is built on".
    CONSTRAINT legs_teasable_market_type
        CHECK (teased_line IS NULL OR market_type IN ('spread', 'total')),

    -- ErrLegLineRequired: a teased leg's underlying price must carry a line, or
    -- there is nothing for the tease to be measured against. Implied by
    -- legs_price_line_rule for spread/total, and stated anyway so the rule is
    -- readable where it is relied on -- the teaser magnitude check in
    -- wagers_assert_shape() would silently evaluate to NULL against a NULL line.
    CONSTRAINT legs_teased_requires_price_line
        CHECK (teased_line IS NULL OR price_line IS NOT NULL),

    -- The market_type copy, pinned. See the header for why ON UPDATE RESTRICT
    -- rather than the catalogue migration's CASCADE.
    CONSTRAINT legs_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE RESTRICT,

    -- validateLegSet's ErrDuplicateSelection: "the same selection appears twice
    -- on one wager, which is a slip-building bug rather than a bet."
    CONSTRAINT legs_wager_selection_key UNIQUE (wager_id, selection_id),

    -- validateLegSet's ErrDuplicateMarket: two legs answering ONE market are
    -- "competing answers to one question -- home and away moneyline, over and
    -- under the same total -- so they cannot both win, and a ticket that requires
    -- both to win is dead on arrival."
    --
    -- Both constraints are kept even though a genuine duplicate selection implies
    -- a duplicate market: legs.market_id cannot be pinned to the selection's own
    -- market (see the cross-agent note at the end of this file), so a malformed
    -- writer could repeat a selection under two different market ids and escape
    -- the market constraint alone. Each index catches a case the other misses.
    CONSTRAINT legs_wager_market_key UNIQUE (wager_id, market_id)
);

CREATE TRIGGER legs_set_updated_at
    BEFORE UPDATE ON legs
    FOR EACH ROW EXECUTE FUNCTION betting_set_updated_at();

COMMENT ON TABLE legs IS
    'One selection on a ticket (domain.Leg), holding THE PRICE AT PLACEMENT TIME as values (CLAUDE.md §4). The price columns are deliberately denormalised and deliberately NOT a foreign key into `prices`; they are also immutable by trigger. Do not normalise them -- read the header of this migration.';
COMMENT ON COLUMN legs.price_decimal IS
    'Price.Decimal() frozen at placement. Never re-resolved from the prices hypertable: a booked leg must grade and pay at the number the customer took, whatever the market did afterwards.';
COMMENT ON COLUMN legs.price_line IS
    'Price.Line() frozen at placement, from this selection''s own perspective (EffectiveLine, already inverted for an away spread). NULL means no line; 0.0 is a traded pick''em.';
COMMENT ON COLUMN legs.price_observed_at IS
    'Price.ObservedAt(): when the QUOTE was observed. Not when the bet was placed -- that is wagers.placed_at.';
COMMENT ON COLUMN legs.teased_line IS
    'Leg.TeasedLine(): the moved line a teaser leg grades at. The untouched market price stays in price_decimal/price_line so line history and CLV are not corrupted by a line the book never traded.';
COMMENT ON COLUMN legs.market_type IS
    'Leg.MarketType() copied at placement, pinned to markets(id, type) by a composite FK with ON UPDATE RESTRICT so a market''s type cannot be changed under a booked ticket.';

-- WHY THERE IS NO SEPARATE INDEX ON legs (wager_id):
-- legs_wager_selection_key and legs_wager_market_key both lead with wager_id, so
-- "the legs of this ticket" -- the wager-history and bet-slip read -- is already
-- an index scan on either.

-- ---------------------------------------------------------------------------
-- wagers and legs: immutability and the two state machines
--
-- These are BEFORE ROW triggers, not privileges. 00007 makes the argument and it
-- holds here: "the application connects as the database owner, and an owner's
-- privileges on its own table are self-restorable -- one GRANT and the protection
-- is gone, silently. A trigger fires for the owner and for a superuser alike."
--
-- What they enforce, and why each one is a database concern rather than only a Go
-- concern:
--
--   1. The booked terms are immutable. Every column except the lifecycle ones is
--      refused on UPDATE. Without this, CLAUDE.md §4's "never a live reference"
--      is one UPDATE away from false -- and the UPDATE would look like a routine
--      correction.
--
--   2. Terminal is terminal. wager.go: "A settled ticket cannot be re-graded,
--      cannot be re-settled at a different amount, and cannot be cashed out ...
--      A mutable terminal state would let a payout be silently rewritten, in the
--      one subsystem whose entire purpose is being auditable." Corrections are
--      EntryKindAdjustment rows in the ledger, which leave the original on the
--      record.
--
--   3. s -> s is legal and idempotent. Kafka is at-least-once (CLAUDE.md §3), so
--      the settle consumer WILL be handed "this leg won" twice as a matter of
--      routine, and making redelivery an error would force every consumer to
--      special-case a healthy system. On a repeat, graded_at / returned_minor
--      keep their ORIGINAL values, exactly as Leg.WithStatus does: "it keeps the
--      ORIGINAL gradedAt rather than advancing it, so a redelivered settlement
--      event cannot move the recorded grading time."
--
--   4. transitioned_at is monotone. This one differs from the catalogue
--      migration's deliberate refusal to enforce monotonicity on observed_at, and
--      the difference is in the domain, not in taste: an out-of-order ODDS update
--      must be a silent no-op, whereas Wager.stamp() RAISES on a stale instant
--      (ErrStaleUpdate). The schema agreeing with the domain's own transition
--      method is not a new policy. A same-instant update is accepted, because
--      "two events can share one".
--
-- DELETE is refused outright on both tables. A placed bet is a money record;
-- 00005 states the principle ("auth history and money movements must survive")
-- and applies it to every reference to users. Note that a committed wager is
-- ALREADY undeletable through a second, structural route, which is worth writing
-- down because it is emergent: its stake transaction in ledger_transactions
-- references it ON DELETE RESTRICT, and that transaction cannot itself be deleted
-- because ledger_transactions is append-only. The trigger closes the remaining
-- window -- a wager whose ledger rows have not been written yet.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION wagers_assert_transition() RETURNS trigger
    LANGUAGE plpgsql
AS $wagers_assert_transition$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION
            'wagers is not deletable: wager % is a money record', OLD.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'A placed bet is permanent. Correct a wrong settlement with an '
                            'EntryKindAdjustment ledger transaction, which leaves the original '
                            'on the record.';
    END IF;

    -- The booked terms. Everything a customer agreed to at placement.
    IF (NEW.id, NEW.user_id, NEW.kind, NEW.stake_minor, NEW.accepted_decimal,
        NEW.rounding, NEW.potential_payout_minor, NEW.potential_profit_minor,
        NEW.teaser_points, NEW.round_robin_id, NEW.placed_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.user_id, OLD.kind, OLD.stake_minor, OLD.accepted_decimal,
        OLD.rounding, OLD.potential_payout_minor, OLD.potential_profit_minor,
        OLD.teaser_points, OLD.round_robin_id, OLD.placed_at, OLD.created_at)
    THEN
        RAISE EXCEPTION
            'wager % booked terms are immutable; only status, transitioned_at and the returned amount may change',
            OLD.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'CLAUDE.md section 4: a leg holds the price at placement time. '
                            'The same rule applies to the ticket it is on.';
    END IF;

    -- WagerStatus.CanTransitionTo(), spelled out. s -> s falls through as legal.
    IF OLD.status <> NEW.status
       AND NOT (
            (OLD.status = 'placed' AND NEW.status IN ('open', 'won', 'lost',
                                                      'void', 'push', 'cashed_out'))
         OR (OLD.status = 'open'   AND NEW.status IN ('won', 'lost', 'void',
                                                      'push', 'cashed_out'))
       )
    THEN
        RAISE EXCEPTION
            'wager %: % -> % is not a legal transition',
            OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'placed -> open | settled; open -> settled; a settled wager is '
                            'terminal. Re-grade by writing an adjustment, never by editing.';
    END IF;

    -- Settlement is written once. Idempotent redelivery must not re-price it.
    IF OLD.returned_minor IS NOT NULL
       AND (NEW.returned_minor, NEW.net_return_minor)
           IS DISTINCT FROM (OLD.returned_minor, OLD.net_return_minor)
    THEN
        RAISE EXCEPTION
            'wager % already settled returning % minor units; the returned amount is write-once',
            OLD.id, OLD.returned_minor
            USING ERRCODE = 'restrict_violation';
    END IF;

    -- Wager.stamp(): "update at %s precedes %s" is ErrStaleUpdate.
    IF NEW.transitioned_at < OLD.transitioned_at THEN
        RAISE EXCEPTION
            'wager %: transition at % precedes the recorded % (stale update)',
            OLD.id, NEW.transitioned_at, OLD.transitioned_at
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$wagers_assert_transition$;
-- +goose StatementEnd

CREATE TRIGGER wagers_assert_transition
    BEFORE UPDATE OR DELETE ON wagers
    FOR EACH ROW EXECUTE FUNCTION wagers_assert_transition();

COMMENT ON FUNCTION wagers_assert_transition() IS
    'BEFORE UPDATE OR DELETE on wagers: booked terms immutable, WagerStatus edges enforced, returned amount write-once, transitioned_at monotone, DELETE refused. SQLSTATE 23001.';

-- +goose StatementBegin
CREATE FUNCTION legs_assert_transition() RETURNS trigger
    LANGUAGE plpgsql
AS $legs_assert_transition$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION
            'legs is not deletable: leg % holds the price its wager was booked at', OLD.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'Removing a leg would rewrite the terms of a placed bet and would '
                            'silently reprice the ticket. Void the leg instead (status = void), '
                            'which is the domain''s own way of removing it from the parlay.';
    END IF;

    -- THE BOOKED PRICE IS IMMUTABLE. This is CLAUDE.md section 4's "never a live
    -- reference" enforced against every writer, not only against the Go type.
    IF (NEW.id, NEW.wager_id, NEW.event_id, NEW.market_id, NEW.market_type,
        NEW.selection_id, NEW.role, NEW.price_book_id, NEW.price_decimal,
        NEW.price_line, NEW.price_observed_at, NEW.teased_line, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.wager_id, OLD.event_id, OLD.market_id, OLD.market_type,
        OLD.selection_id, OLD.role, OLD.price_book_id, OLD.price_decimal,
        OLD.price_line, OLD.price_observed_at, OLD.teased_line, OLD.created_at)
    THEN
        RAISE EXCEPTION
            'leg % is immutable once booked; only status and graded_at may change',
            OLD.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'A leg holds the price at placement time. Editing it grades the '
                            'customer at a handicap they never took.';
    END IF;

    -- LegStatus.CanTransitionTo(): pending -> terminal, terminal is terminal,
    -- s -> s legal.
    IF OLD.status <> NEW.status AND OLD.status <> 'pending' THEN
        RAISE EXCEPTION
            'leg % was already graded %; a graded leg is terminal',
            OLD.id, OLD.status
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'A corrected result is an EntryKindAdjustment in the ledger, which '
                            'leaves both the original grading and the correction on the record.';
    END IF;

    -- Leg.WithStatus on a repeat: "it keeps the ORIGINAL gradedAt rather than
    -- advancing it, so a redelivered settlement event cannot move the recorded
    -- grading time."
    IF OLD.graded_at IS NOT NULL AND NEW.graded_at IS DISTINCT FROM OLD.graded_at THEN
        RAISE EXCEPTION
            'leg % was graded at %; graded_at is write-once',
            OLD.id, OLD.graded_at
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$legs_assert_transition$;
-- +goose StatementEnd

CREATE TRIGGER legs_assert_transition
    BEFORE UPDATE OR DELETE ON legs
    FOR EACH ROW EXECUTE FUNCTION legs_assert_transition();

COMMENT ON FUNCTION legs_assert_transition() IS
    'BEFORE UPDATE OR DELETE on legs: the booked price and every relationship key are immutable, LegStatus edges enforced, graded_at write-once, DELETE refused. SQLSTATE 23001.';

-- ---------------------------------------------------------------------------
-- wagers <-> legs: the cross-row shape rules, as a DEFERRED constraint trigger
--
-- Four rules from the domain cannot be expressed as CHECK constraints because
-- every one of them is a statement about a SET of legs rather than about a row:
--
--   1. Arity.       validateLegCount: a straight has exactly 1 leg; a parlay,
--                   round robin or teaser has at least 2; nothing exceeds
--                   domain.MaxWagerLegs (25). A wager with no legs at all is a
--                   ticket on nothing.
--   2. Teaser.      validateTeaser: on a teaser EVERY leg carries a teased line
--                   the promised number of points off the line it was booked at;
--                   on anything else NO leg is teased.
--   3. Straight.    validateTicketPrice: a straight's ticket price equals its
--                   single leg's quoted price. wager.go calls a mismatch
--                   something that "can only be an arithmetic or plumbing fault".
--   4. Round robin. A round-robin ticket's leg count is one of the combination
--                   sizes its parent declared.
--
-- WHY DEFERRED, and why that is not a detail: a wager and its legs arrive as
-- separate INSERT statements in one database transaction, and the wager row must
-- be written first for the legs' foreign key to resolve. An immediate check would
-- reject the wager for having no legs before its legs could possibly exist.
-- DEFERRABLE INITIALLY DEFERRED moves the check to COMMIT, where the whole ticket
-- is visible. This is the identical lesson to the ledger trigger below, and it is
-- the reason both are constraint triggers rather than ordinary ones.
--
-- THE ALTERNATIVE CONSIDERED AND REJECTED: denormalise wagers.kind and
-- wagers.teaser_points onto `legs` and pin them with composite foreign keys, the
-- way legs.market_type is pinned -- which would turn rule 2 into a plain CHECK.
-- Rejected because rules 1, 3 and 4 are aggregates that no CHECK can express with
-- or without the copy, so the trigger has to exist anyway; the copy would add two
-- columns and two indexes to the busiest betting table to relocate one of four
-- rules. Where a copy DOES buy something no trigger can (legs.market_type, so a
-- leg is interpretable without reading its market) it is used.
--
-- Constraint triggers must be AFTER ... FOR EACH ROW -- PostgreSQL admits no
-- statement-level form -- so the cost is one index-only aggregate over a
-- handful of rows per inserted leg. Measured against the alternative of a wrong
-- ticket, that is not a cost worth optimising.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION wagers_assert_shape() RETURNS trigger
    LANGUAGE plpgsql
AS $wagers_assert_shape$
DECLARE
    w        wagers;
    w_id     TEXT;
    n_legs   INTEGER;
    n_teased INTEGER;
    leg_odds DOUBLE PRECISION;
    bad_leg  TEXT;
BEGIN
    -- Resolve the wager this row belongs to. Nested IF blocks rather than a CASE
    -- expression, for the reason documented at length in
    -- ledger_assert_transaction_balanced(): a CASE is one SQL expression, so
    -- PL/pgSQL resolves the untaken branch's field reference too and the trigger
    -- dies with `record "new" has no field "wager_id"` when it fires on wagers.
    IF TG_TABLE_NAME = 'wagers' THEN
        IF TG_OP = 'DELETE' THEN
            w_id := OLD.id;
        ELSE
            w_id := NEW.id;
        END IF;
    ELSE
        IF TG_OP = 'DELETE' THEN
            w_id := OLD.wager_id;
        ELSE
            w_id := NEW.wager_id;
        END IF;
    END IF;

    -- The whole ticket may have been removed within this same transaction, in
    -- which case there is no shape left to constrain. Unreachable while the
    -- delete guards above are installed; kept so this trigger is correct on its
    -- own terms rather than only in combination with them.
    SELECT * INTO w FROM wagers WHERE id = w_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;

    SELECT count(*),
           count(*) FILTER (WHERE teased_line IS NOT NULL)
      INTO n_legs, n_teased
      FROM legs
     WHERE wager_id = w_id;

    -- 1. validateLegCount + validateLegSet's bounds.
    IF n_legs = 0 THEN
        RAISE EXCEPTION 'wager % has no legs', w_id
            USING ERRCODE = 'check_violation',
                  HINT    = 'A ticket with no selections is a bet on nothing. Insert the wager '
                            'and its legs in ONE transaction; this check is deferred to COMMIT '
                            'precisely so that ordering works.';
    END IF;
    IF w.kind = 'straight' AND n_legs <> 1 THEN
        RAISE EXCEPTION 'wager % is a straight with % legs', w_id, n_legs
            USING ERRCODE = 'check_violation';
    END IF;
    IF w.kind IN ('parlay', 'round_robin', 'teaser') AND n_legs < 2 THEN
        RAISE EXCEPTION 'wager % is a % with % leg(s)', w_id, w.kind, n_legs
            USING ERRCODE = 'check_violation';
    END IF;
    IF n_legs > 25 THEN
        RAISE EXCEPTION 'wager % has % legs; domain.MaxWagerLegs is 25', w_id, n_legs
            USING ERRCODE = 'check_violation';
    END IF;

    -- 2. validateTeaser. Presence first, then the magnitude of every tease.
    IF w.kind = 'teaser' THEN
        IF n_teased <> n_legs THEN
            RAISE EXCEPTION
                'wager % is a teaser but only % of % legs carry a teased line',
                w_id, n_teased, n_legs
                USING ERRCODE = 'check_violation';
        END IF;

        -- The check that earns its place, per validateTeaser: a leg teased by the
        -- wrong amount -- or in the wrong DIRECTION, which is the same magnitude
        -- with the wrong sign and therefore the more dangerous error -- is
        -- invisible without it, because both lines are individually plausible.
        --
        -- 1e-9 is domain.teaserLineTolerance, and it is ABSOLUTE because the
        -- quantity compared is a difference of two lines: lines are quarter-point
        -- multiples and teaser points half-point multiples, all dyadic and
        -- therefore exact in float64, so a correct tease differs from the promise
        -- by exactly zero. The tolerance absorbs a value that arrived through a
        -- decimal string parse; the smallest real mismatch expressible is a
        -- quarter point, 2.5e8 times this tolerance.
        SELECT id INTO bad_leg
          FROM legs
         WHERE wager_id = w_id
           AND abs(abs(teased_line - price_line) - w.teaser_points) > 1e-9
         LIMIT 1;
        IF bad_leg IS NOT NULL THEN
            RAISE EXCEPTION
                'wager % leg % is not teased by the stated % points', w_id, bad_leg, w.teaser_points
                USING ERRCODE = 'check_violation',
                      HINT    = 'A mis-teased leg grades at a handicap nobody sold.';
        END IF;

        -- DIRECTION, and this check goes ONE STEP BEYOND validateTeaser's
        -- implementation on purpose. Read the reason before removing it.
        --
        -- domain.WagerKindTeaser is defined as "two or more spread or total
        -- selections whose lines are all moved IN THE BETTOR'S FAVOUR by the same
        -- number of points", and validateTeaser's own doc comment says its last
        -- check exists to catch a leg "teased in the wrong DIRECTION, which is the
        -- same magnitude with the wrong sign and therefore the more dangerous
        -- error".
        --
        -- Its implementation cannot do that. `math.Abs(math.Abs(teased-booked) -
        -- points)` is symmetric in the sign of (teased - booked), so a home spread
        -- of -3.5 teased to -9.5 -- six points AGAINST the customer -- satisfies it
        -- exactly. This was found by attacking the database, not by reading the
        -- code, and it is reported to the domain's owner rather than patched here.
        --
        -- The rule this enforces is total, because price_line is already stated
        -- from the SELECTION's own perspective (EffectiveLine inverts the away
        -- spread before the price is written):
        --
        --   role = 'over'   the threshold moves DOWN   teased = line - points
        --   home, away      points are added to your side
        --   role = 'under'  the threshold moves UP     teased = line + points
        --
        -- Only spread and total legs can be teased (legs_teasable_market_type), so
        -- those four roles are the whole domain of the CASE.
        --
        -- The consequence is deliberate and worth stating: for a wrong-direction
        -- tease, Go's NewWager succeeds and this INSERT fails. That is the database
        -- catching a bug the type system currently lets through, which is exactly
        -- the reason CLAUDE.md section 4's invariants are enforced in both places.
        SELECT id INTO bad_leg
          FROM legs
         WHERE wager_id = w_id
           AND abs(teased_line - (CASE WHEN role = 'over'
                                       THEN price_line - w.teaser_points
                                       ELSE price_line + w.teaser_points
                                  END)) > 1e-9
         LIMIT 1;
        IF bad_leg IS NOT NULL THEN
            RAISE EXCEPTION
                'wager % leg % is teased AGAINST the bettor: % points from the booked line the wrong way',
                w_id, bad_leg, w.teaser_points
                USING ERRCODE = 'check_violation',
                      HINT    = 'A teaser moves every line in the bettor''s favour. An over''s '
                                'threshold moves down; a spread or an under moves up. The '
                                'magnitude test above cannot see a sign error, so this is a '
                                'separate check.';
        END IF;
    ELSIF n_teased > 0 THEN
        RAISE EXCEPTION
            'wager % is a % but % of its legs carry a teased line',
            w_id, w.kind, n_teased
            USING ERRCODE = 'check_violation';
    END IF;

    -- 3. validateTicketPrice for a straight. The relative tolerance and its
    --    justification are the domain's: "The two numbers are meant to be the
    --    same value travelling by two routes, so anything past a few thousand
    --    ULPs is a plumbing fault rather than accumulated rounding."
    IF w.kind = 'straight' THEN
        SELECT price_decimal INTO leg_odds FROM legs WHERE wager_id = w_id;
        IF abs(w.accepted_decimal - leg_odds)
           / greatest(abs(w.accepted_decimal), abs(leg_odds)) > 1e-12 THEN
            RAISE EXCEPTION
                'wager % is priced at % on a leg quoted %',
                w_id, w.accepted_decimal, leg_odds
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- 4. A round-robin ticket is one COMBINATION of its parent, so its leg count
    --    is one of the sizes the parent declared.
    --
    --    Deliberately NOT checked: that the number of tickets equals
    --    RoundRobin.CombinationCount(), the sum of C(n,k) over those sizes.
    --    Evaluating binomial coefficients in plpgsql to admit an INSERT would be
    --    a second implementation of RoundRobin.Combinations(), and CLAUDE.md
    --    section 10's rule about two implementations of one formula applies. It
    --    is a reconciliation query for phase 9, where a mismatch is a report
    --    rather than a rejected bet.
    IF w.kind = 'round_robin'
       AND NOT EXISTS (SELECT 1 FROM round_robin_sizes
                        WHERE round_robin_id = w.round_robin_id
                          AND size = n_legs)
    THEN
        RAISE EXCEPTION
            'wager % has % legs, which is not a combination size declared by round robin %',
            w_id, n_legs, w.round_robin_id
            USING ERRCODE = 'check_violation';
    END IF;

    RETURN NULL;
END;
$wagers_assert_shape$;
-- +goose StatementEnd

COMMENT ON FUNCTION wagers_assert_shape() IS
    'Deferred constraint trigger body: leg-count arity per wager kind, teaser correspondence and magnitude, a straight''s price against its leg, and a round-robin ticket''s size against its parent. Runs at COMMIT because a wager and its legs are separate statements.';

CREATE CONSTRAINT TRIGGER wagers_shape_at_commit
    AFTER INSERT OR UPDATE OR DELETE ON wagers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wagers_assert_shape();

CREATE CONSTRAINT TRIGGER legs_shape_at_commit
    AFTER INSERT OR UPDATE OR DELETE ON legs
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION wagers_assert_shape();

-- ---------------------------------------------------------------------------
-- TRUNCATE guards for the betting tables
--
-- A constraint trigger cannot fire on TRUNCATE, and neither can a row-level
-- one. Without a statement-level guard, `TRUNCATE legs` would leave every wager
-- with zero legs and every deferred check above satisfied -- the invariants would
-- simply have nothing left to be true about. 00005 and 00007 install the same
-- guard on their append-only tables for the same reason.
--
-- The reset button for a development database is `docker compose down -v`
-- (CLAUDE.md section 9: "named volumes for all persistent state so
-- `docker compose down -v` is the reset button"), and integration tests get a
-- fresh database per run from testcontainers. Neither needs TRUNCATE.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION betting_reject_truncate() RETURNS trigger
    LANGUAGE plpgsql
AS $betting_reject_truncate$
BEGIN
    RAISE EXCEPTION
        'TRUNCATE is refused on %: it would remove money records while satisfying every deferred check',
        TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation',
              HINT    = 'Reset a development database with `docker compose down -v`.';
END;
$betting_reject_truncate$;
-- +goose StatementEnd

CREATE TRIGGER wagers_no_truncate
    BEFORE TRUNCATE ON wagers
    FOR EACH STATEMENT EXECUTE FUNCTION betting_reject_truncate();

CREATE TRIGGER legs_no_truncate
    BEFORE TRUNCATE ON legs
    FOR EACH STATEMENT EXECUTE FUNCTION betting_reject_truncate();

CREATE TRIGGER round_robins_no_truncate
    BEFORE TRUNCATE ON round_robins
    FOR EACH STATEMENT EXECUTE FUNCTION betting_reject_truncate();

CREATE TRIGGER round_robin_sizes_no_truncate
    BEFORE TRUNCATE ON round_robin_sizes
    FOR EACH STATEMENT EXECUTE FUNCTION betting_reject_truncate();

-- ---------------------------------------------------------------------------
-- ledger_transactions
--
-- domain.Transaction: "a set of entries that sum to exactly zero."
--
-- Named `ledger_transactions` rather than `transactions` on purpose. In a
-- database, "transaction" already means something else, and every sentence about
-- this table would need a qualifier forever. The prefix also pairs it visibly
-- with ledger_entries, which is how the domain groups them (LedgerEntry, Balance,
-- LedgerIsBalanced).
--
-- NO updated_at, AND NO updated_at TRIGGER. The frozen convention asks for
-- updated_at on MUTABLE tables. This one is append-only: UPDATE, DELETE and
-- TRUNCATE are all refused by trigger below. A ledger that can be edited is not a
-- ledger.
--
-- WHY THERE IS NO `accounts` TABLE FOR THESE ENTRIES TO REFERENCE
-- ---------------------------------------------------------------
-- ledger.go: "An account IS its (kind, owner) pair, exactly as a Price is its
-- (selection, book, instant) and for the same reason: a surrogate key would add a
-- uniqueness constraint to maintain and an allocation step to run, without
-- answering any question the natural key does not."
--
-- So an entry carries (account_kind, account_user_id) and there is nothing to
-- join to. The four account kinds are a CHECK, not four seeded rows -- which also
-- means there is no chart of accounts to keep in step with AccountKind, and no
-- migration that has to insert a row before the first grant can be made.
-- ---------------------------------------------------------------------------
CREATE TABLE ledger_transactions (
    id          TEXT        PRIMARY KEY
                            CONSTRAINT ledger_transactions_id_charset
                            CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.EntryKind.String(). Transaction.Kind(): why the money moved. Every
    -- entry carries the same kind, pinned by the composite FK from
    -- ledger_entries.
    kind        TEXT        NOT NULL
                            CONSTRAINT ledger_transactions_kind_defined
                            CHECK (kind IN ('grant', 'stake', 'payout', 'loss',
                                            'refund', 'cash_out', 'adjustment')),

    -- Transaction.WagerID(), and whether there is one. RESTRICT: the settlement
    -- audit trail must outlive any attempt to delete the ticket it settles. This
    -- FK, together with this table's append-only guard, is what makes a committed
    -- wager structurally undeletable.
    wager_id    TEXT
                            REFERENCES wagers (id) ON DELETE RESTRICT
                            CONSTRAINT ledger_transactions_wager_id_charset
                            CHECK (wager_id IS NULL
                                   OR wager_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Transaction.OccurredAt(), UTC: when the movement HAPPENED, from the acting
    -- service's clock. A domain instant, passed explicitly into every
    -- constructor. Never now(), and no trigger touches it.
    occurred_at TIMESTAMPTZ NOT NULL
                            CONSTRAINT ledger_transactions_occurred_at_sane
                            CHECK (occurred_at > '1900-01-01T00:00:00Z'),

    -- When this row was WRITTEN, from the database clock. Makes replay visible:
    -- a settle consumer re-reading a Kafka partition writes rows whose created_at
    -- is hours after occurred_at. 00007 draws the same distinction on audit_log,
    -- and for the same reason declines to CHECK one against the other -- a few
    -- milliseconds of skew between two containers must not reject a ledger write.
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Which movements are ABOUT a wager, from the constructors in ledger.go:
    --
    --   NewGrantTransaction       never passes a WagerID -- a grant is money
    --                             entering the system, not a bet
    --   NewStakeTransaction       always passes w.ID()
    --   NewSettlementTransaction  always passes w.ID() (payout, loss, refund,
    --                             cash_out)
    --   NewAdjustmentTransaction  takes one and documents that it may be the zero
    --                             value: "Pass the zero WagerID when the
    --                             correction is not about a wager."
    CONSTRAINT ledger_transactions_wager_matches_kind
        CHECK (CASE kind
                   WHEN 'grant'      THEN wager_id IS NULL
                   WHEN 'adjustment' THEN TRUE
                   WHEN 'stake'      THEN wager_id IS NOT NULL
                   WHEN 'payout'     THEN wager_id IS NOT NULL
                   WHEN 'loss'       THEN wager_id IS NOT NULL
                   WHEN 'refund'     THEN wager_id IS NOT NULL
                   WHEN 'cash_out'   THEN wager_id IS NOT NULL
                   ELSE FALSE
               END),

    -- Target for the composite FK on ledger_entries. ONE unique constraint, three
    -- columns, pinning BOTH values an entry denormalises -- see that table.
    CONSTRAINT ledger_transactions_id_kind_occurred_key
        UNIQUE (id, kind, occurred_at)
);

COMMENT ON TABLE ledger_transactions IS
    'One balanced money movement (domain.Transaction). Append-only: UPDATE, DELETE and TRUNCATE are refused by trigger. Its entries are guaranteed to sum to exactly zero by a DEFERRED constraint trigger that fires at COMMIT.';
COMMENT ON COLUMN ledger_transactions.occurred_at IS
    'Event time: when the movement happened, from the acting service''s clock (Transaction.OccurredAt). Never now().';
COMMENT ON COLUMN ledger_transactions.created_at IS
    'Ingestion time: when this row was written, from the database clock. Differs from occurred_at on Kafka replay.';
COMMENT ON COLUMN ledger_transactions.wager_id IS
    'The wager this movement settles, where there is one. A grant has none; an operator adjustment may have none. ON DELETE RESTRICT, which together with the append-only guard makes a wager with ledger history undeletable.';

-- ---------------------------------------------------------------------------
-- ledger_entries
--
-- domain.LedgerEntry: "one signed movement against one account ... half of a
-- movement, never a whole one."
--
-- SIGN CONVENTION, from ledger.go, because reading a row is meaningless without
-- it: "A positive amount CREDITS the account and a negative amount DEBITS it. For
-- a user's cash that reads the way a customer expects -- positive means they have
-- more. For issuance it runs the other way and should: money created leaves that
-- account, so its balance is negative and its magnitude is the currency in
-- circulation. Every entry in the system summed together is exactly zero."
--
-- THE PRIMARY KEY IS (transaction_id, entry_index), AND THAT IS NOT A SURROGATE
-- -----------------------------------------------------------------------------
-- A LedgerEntry has no identifier in the domain, and it needs none: nothing in
-- the system ever references one entry. `entry_index` is its ORDINAL WITHIN ITS
-- TRANSACTION -- the position it already occupies in Transaction.entries, which
-- is an ordered slice that Entries() returns in order. Selecting
-- ORDER BY entry_index therefore rehydrates Transaction.Entries() exactly, which
-- matters because NewSettlementTransaction builds its entries in a documented
-- order (escrow release, customer credit, house delta).
--
-- The alternatives were both worse on the busiest write path in this schema. A
-- bigserial is a sequence, and therefore a contention point, for a number nobody
-- reads. A UUID costs 16 bytes and randomness for the same nobody -- 00007 chose
-- one for audit_log.id only because the WRITER needs the id before the INSERT, to
-- name it in a log line and a span attribute; no such caller exists here.
--
-- Note what the key does NOT constrain: several entries of one transaction may
-- touch the SAME account. Transaction.NetFor "sums rather than finds" precisely
-- because of that, so there is deliberately no unique constraint on
-- (transaction_id, account_kind, account_user_id).
--
-- WHY kind AND occurred_at ARE DUPLICATED FROM THE PARENT
-- -------------------------------------------------------
-- ledger.go on the kind: "The kind is carried on the entry as well as the
-- transaction, and the two are checked to agree. That is one redundant byte per
-- row, spent so that the stored rows are self-describing: 'sum every stake entry
-- this month' is a query against one table rather than a join, and a row that
-- arrives without its transaction is still interpretable."
--
-- `occurred_at` is carried for the same reason and pays for itself in the same
-- query. Both of 00005's money-limit evaluations are period-scoped sums over
-- entries ("sum of ledger_entries.amount_minor where kind='grant' and the account
-- is the user's cash, within the period"), and with both columns present that is
-- one index-only range scan on ledger_entries_account_idx instead of a join.
--
-- Neither copy can drift. The composite foreign key
-- (transaction_id, kind, occurred_at) -> ledger_transactions (id, kind,
-- occurred_at) makes a row that disagrees with its parent unstorable, and its
-- default ON UPDATE NO ACTION makes those parent columns immutable while any
-- entry references them. One constraint, one index, both copies pinned.
--
-- INSERT ORDER: the transaction row first, then its entries, in ONE database
-- transaction. The foreign key is immediate (not deferred), so an entry cannot be
-- written before the movement it belongs to -- an entry without its transaction
-- is not a partial record, it is an uninterpretable one.
--
-- WHY THIS IS NOT A HYPERTABLE, although it is append-only and time-ordered and
-- looks exactly like `prices` and `audit_log`:
-- BECAUSE A HYPERTABLE IS DROPPABLE BY CHUNK. 00007 makes that a FEATURE for
-- audit_log -- drop_chunks is DROP TABLE and fires no row trigger, so retention
-- becomes the one sanctioned way to remove history. For the ledger it would be a
-- catastrophe: dropping a chunk would delete one side of every movement that
-- straddles its boundary, silently unbalancing the ledger that the trigger below
-- exists to keep balanced, and no retention policy can ever be correct for the
-- system's own source of truth. This table grows forever. That is the point.
-- ---------------------------------------------------------------------------
CREATE TABLE ledger_entries (
    transaction_id  TEXT        NOT NULL
                                CONSTRAINT ledger_entries_transaction_id_charset
                                CHECK (transaction_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Position within Transaction.entries. See the header: an ordinal, not a
    -- surrogate identity.
    entry_index     INTEGER     NOT NULL
                                CONSTRAINT ledger_entries_entry_index_range
                                CHECK (entry_index >= 0),

    -- domain.AccountKind.String().
    account_kind    TEXT        NOT NULL
                                CONSTRAINT ledger_entries_account_kind_defined
                                CHECK (account_kind IN ('user_cash', 'user_escrow',
                                                        'house', 'issuance')),

    -- Account.Owner(). RESTRICT: a customer's money history must survive any
    -- attempt to delete them, which is the same rule 00005 applies to every
    -- reference to users but the TOTP row.
    account_user_id TEXT
                                REFERENCES users (id) ON DELETE RESTRICT
                                CONSTRAINT ledger_entries_account_user_id_charset
                                CHECK (account_user_id IS NULL
                                       OR account_user_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- LedgerEntry.Amount(): the SIGNED movement, positive credits and negative
    -- debits. BIGINT minor units, bounded by domain.MaxSafeMoney in both
    -- directions.
    --
    -- NEVER ZERO. NewLedgerEntry refuses a zero amount (ErrZeroEntryAmount):
    -- "Zero-amount rows balance trivially and would let a 'transaction' be filed
    -- that moved no money, which is noise in the one table that must be
    -- readable." NewSettlementTransaction drops the entries that come out to zero
    -- rather than storing them, which is why a refund has two entries and a win
    -- has three.
    amount_minor    BIGINT      NOT NULL
                                CONSTRAINT ledger_entries_amount_range
                                CHECK (amount_minor <> 0
                                       AND amount_minor >= -9007199254740991
                                       AND amount_minor <= 9007199254740991),

    -- domain.EntryKind.String(), pinned to the parent's by the composite FK.
    kind            TEXT        NOT NULL
                                CONSTRAINT ledger_entries_kind_defined
                                CHECK (kind IN ('grant', 'stake', 'payout', 'loss',
                                                'refund', 'cash_out', 'adjustment')),

    -- Denormalised Transaction.OccurredAt(), pinned by the same composite FK.
    occurred_at     TIMESTAMPTZ NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT ledger_entries_pkey PRIMARY KEY (transaction_id, entry_index),

    -- NewAccount's two rules, as one biconditional:
    --   AccountKind.IsUserOwned() -> ErrAccountOwnerRequired without an owner
    --   AccountKind.IsSystem()    -> ErrAccountOwnerNotApplicable with one
    -- ledger.go on the second: "the house and the issuance account are
    -- singletons; one per user would make 'what is the book's position' a sum
    -- nobody remembers to take."
    CONSTRAINT ledger_entries_owner_matches_account_kind
        CHECK ((account_kind IN ('user_cash', 'user_escrow'))
               = (account_user_id IS NOT NULL)),

    -- The one constraint that pins both denormalised copies. RESTRICT is
    -- redundant with the append-only guard and stated anyway, so the intent
    -- survives if the guard is ever revisited.
    CONSTRAINT ledger_entries_transaction_fk
        FOREIGN KEY (transaction_id, kind, occurred_at)
        REFERENCES ledger_transactions (id, kind, occurred_at)
        ON DELETE RESTRICT
);

COMMENT ON TABLE ledger_entries IS
    'One signed half of a money movement (domain.LedgerEntry). Append-only, never a hypertable, and there is no balance column anywhere -- a balance is the fold in the account_balances view. Entries of one transaction are guaranteed to sum to exactly zero at COMMIT.';
COMMENT ON COLUMN ledger_entries.amount_minor IS
    'BIGINT minor units, SIGNED: positive credits the account, negative debits it. Never zero -- an entry that moves nothing is not a movement.';
COMMENT ON COLUMN ledger_entries.entry_index IS
    'Ordinal within Transaction.entries, not a surrogate key. ORDER BY entry_index rehydrates Transaction.Entries() in the order the domain built them.';
COMMENT ON COLUMN ledger_entries.account_kind IS
    'domain.AccountKind. An account IS its (kind, owner) pair, so there is no accounts table and nothing to join to.';
COMMENT ON COLUMN ledger_entries.account_user_id IS
    'Account owner for user_cash / user_escrow; NULL for the house and issuance singletons. The biconditional is enforced.';
COMMENT ON COLUMN ledger_entries.occurred_at IS
    'Denormalised Transaction.OccurredAt(), pinned to the parent by the composite FK so it cannot drift. Present so period-scoped sums (responsible-gaming limits, CLV windows) need no join.';

-- =============================================================================
-- THE ZERO-SUM CONSTRAINT
--
-- CLAUDE.md §4: "double-entry. Every stake, payout, void, and adjustment is two
-- rows that sum to zero."
--
-- Phase 1 made an unbalanced transaction UNCONSTRUCTIBLE in Go. NewTransaction
-- sums the entries and refuses anything but zero, the fields are unexported and
-- there are no setters, so -- as ledger.go puts it -- "there is no expression in
-- the program that produces an unbalanced Transaction value: not a struct
-- literal, not a decode, not a later mutation."
--
-- THAT IS NOT ENOUGH, and the gap is the entire reason this trigger exists. The
-- Go guarantee covers exactly one writer. The database must reject an unbalanced
-- movement no matter what writes it: this migration, an operator at `make psql`,
-- a hand-written sqlc query, a future service nobody has designed yet, a COPY
-- from a repair script, or a bug in the disciplined writer itself. ledger.go
-- anticipates precisely this: LedgerIsBalanced "is the audit hook that will still
-- be true after phase 2 rebuilds these values out of database rows, where the
-- type system's guarantee no longer reaches."
--
-- WHY DEFERRABLE INITIALLY DEFERRED, and why it is the whole design
-- -----------------------------------------------------------------
-- A balanced movement is at least TWO ROWS, and two rows arrive as two
-- statements. After the first INSERT the ledger is unbalanced -- necessarily, and
-- correctly. A NOT DEFERRABLE trigger fires at the end of each statement and
-- would reject that first INSERT, making a correct double-entry write impossible
-- and forcing every writer into a single multi-row INSERT or a temporary table.
--
-- DEFERRABLE INITIALLY DEFERRED moves the check to COMMIT. At that instant the
-- whole movement is visible and the question "does this transaction sum to zero"
-- is finally well-posed. It also closes a route no per-statement check can: a
-- transaction that is balanced after statement 2 and unbalanced again after
-- statement 5 is rejected, because only the FINAL state is examined.
--
-- Constraint triggers are AFTER ... FOR EACH ROW by definition -- PostgreSQL
-- offers no statement-level form -- so a two-entry movement evaluates the
-- aggregate three times (once per entry, once for the transaction row). Each is
-- an index-only lookup on the primary key's leading column against two or three
-- rows. That is the price of the most important invariant in the schema.
--
-- WHY THE TRIGGER IS ALSO ON ledger_transactions
-- -----------------------------------------------
-- Because a trigger on the entries alone cannot see a movement that has NO
-- entries. `INSERT INTO ledger_transactions ...` and nothing else would commit a
-- transaction that moved no money at all and satisfied every check, since no
-- entry row ever existed to fire one. The parent-side trigger closes that, and
-- also enforces ErrTooFewEntries -- "One entry cannot balance unless it is zero,
-- and a zero entry is not a movement of money."
--
-- ROUTES THIS CLOSES. Every one of them is exercised against a live database:
--   * a single unbalanced INSERT
--   * a multi-row VALUES INSERT that does not sum to zero
--   * INSERT ... SELECT
--   * COPY (which fires row triggers, unlike TRUNCATE)
--   * a transaction row with one entry, or with none at all
--   * an UPDATE that unbalances a previously balanced transaction
--   * a DELETE of one side of a balanced pair
--   * a transaction balanced at statement 2 and unbalanced at statement 3
--   * TRUNCATE -- via a separate statement-level guard, because no constraint
--     trigger can fire on it
--
-- The last three are ALSO refused by the append-only guard, which fires first.
-- Both layers are kept: the append-only guard states "the ledger is history", the
-- balance trigger states "the ledger balances", and neither is a substitute for
-- the other. Disabling one still leaves the invariant defended.
-- =============================================================================
-- +goose StatementBegin
CREATE FUNCTION ledger_assert_transaction_balanced() RETURNS trigger
    LANGUAGE plpgsql
AS $ledger_assert_transaction_balanced$
DECLARE
    txn_id TEXT;
    -- NUMERIC, not BIGINT: sum(bigint) returns numeric in PostgreSQL, and
    -- assigning it to a bigint would reintroduce an overflow this check exists to
    -- be exact about. The comparison against 0 is exact for both.
    total  NUMERIC;
    n      INTEGER;
BEGIN
    -- Resolve the transaction this row belongs to. One function serves both
    -- tables, so the field name differs by table and the pseudo-record differs by
    -- operation: NEW is unassigned on DELETE, and referencing it there raises.
    --
    -- Written as nested IF blocks rather than the obvious
    --     txn_id := CASE WHEN TG_TABLE_NAME = 'ledger_entries'
    --                    THEN NEW.transaction_id ELSE NEW.id END;
    -- because that form does not work and fails in a way worth recording. A CASE
    -- is a single SQL expression, so PL/pgSQL resolves EVERY field reference in it
    -- against the actual record -- including the untaken branch -- and the trigger
    -- dies on ledger_transactions with `record "new" has no field
    -- "transaction_id"`. A branch that is never EXECUTED is never resolved; a
    -- branch that is merely never selected inside one expression still is.
    IF TG_TABLE_NAME = 'ledger_entries' THEN
        IF TG_OP = 'DELETE' THEN
            txn_id := OLD.transaction_id;
        ELSE
            txn_id := NEW.transaction_id;
        END IF;
    ELSE
        IF TG_OP = 'DELETE' THEN
            txn_id := OLD.id;
        ELSE
            txn_id := NEW.id;
        END IF;
    END IF;

    -- The whole movement may have been removed within this same transaction.
    -- Removing a balanced movement in full leaves the ledger balanced, so there
    -- is nothing to assert. Unreachable while the append-only guard is installed.
    IF NOT EXISTS (SELECT 1 FROM ledger_transactions WHERE id = txn_id) THEN
        RETURN NULL;
    END IF;

    SELECT coalesce(sum(amount_minor), 0), count(*)
      INTO total, n
      FROM ledger_entries
     WHERE transaction_id = txn_id;

    -- ErrTooFewEntries.
    IF n < 2 THEN
        RAISE EXCEPTION
            'ledger transaction % has % entr(ies); a balanced transaction has at least two',
            txn_id, n
            USING ERRCODE = 'check_violation',
                  HINT    = 'One entry cannot balance unless it is zero, and a zero entry is '
                            'not a movement of money. Write both halves in the same database '
                            'transaction; this check is deferred to COMMIT so that ordering works.';
    END IF;

    -- ErrUnbalancedTransaction. Exact integer equality, which is the right test
    -- here and is why CLAUDE.md section 12 puts money in integers.
    IF total <> 0 THEN
        RAISE EXCEPTION
            'ledger transaction % is UNBALANCED: % entries summing to % minor units, must be exactly 0',
            txn_id, n, total
            USING ERRCODE = 'check_violation',
                  HINT    = 'CLAUDE.md section 4: every movement is two rows that sum to zero. '
                            'Direction is expressed by which account is debited, never by the '
                            'sign of an argument -- see domain.transferEntries.';
    END IF;

    RETURN NULL;
END;
$ledger_assert_transaction_balanced$;
-- +goose StatementEnd

COMMENT ON FUNCTION ledger_assert_transaction_balanced() IS
    'Deferred constraint trigger body: at COMMIT, every ledger transaction must have at least two entries summing to exactly zero minor units. SQLSTATE 23514. The single most important invariant in this schema.';

-- On the entries: catches every INSERT, and (if the append-only guard is ever
-- relaxed) every UPDATE and DELETE that would unbalance a movement.
CREATE CONSTRAINT TRIGGER ledger_entries_balanced_at_commit
    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();

-- On the transactions: catches a movement written with one entry, or with none.
CREATE CONSTRAINT TRIGGER ledger_transactions_balanced_at_commit
    AFTER INSERT OR UPDATE ON ledger_transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();

-- ---------------------------------------------------------------------------
-- Ledger append-only enforcement
--
-- The same argument 00007 makes for audit_log, applied to the table that is the
-- system's source of truth rather than its evidence: "An audit log that the
-- application can edit is not evidence, it is a table with a suggestive name."
--
-- A ledger is stricter still. audit_log admits a retention escape hatch by
-- design -- drop_chunks removes a whole time range. This table has none: it is
-- not a hypertable (see its header) and there is no legitimate operation that
-- removes a ledger row, ever. History is corrected the way ledger.go says:
-- "EntryKindAdjustment ... is how history is corrected WITHOUT rewriting it: the
-- original entries stay on the record and the adjustment sits beside them."
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION ledger_assert_append_only() RETURNS trigger
    LANGUAGE plpgsql
AS $ledger_assert_append_only$
BEGIN
    RAISE EXCEPTION
        'the ledger is append-only: % on % is not permitted', TG_OP, TG_TABLE_NAME
        USING ERRCODE = 'restrict_violation',
              HINT    = 'Correct a movement by writing a compensating adjustment transaction, '
                        'never by editing or removing the original. Balances are derived from '
                        'these rows, so a deleted row is a wrong balance forever.';
    RETURN NULL;
END;
$ledger_assert_append_only$;
-- +goose StatementEnd

COMMENT ON FUNCTION ledger_assert_append_only() IS
    'Guard trigger: raises on any UPDATE, DELETE or TRUNCATE against ledger_entries or ledger_transactions. SQLSTATE 23001 (restrict_violation).';

CREATE TRIGGER ledger_entries_no_update
    BEFORE UPDATE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_append_only();

CREATE TRIGGER ledger_entries_no_delete
    BEFORE DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_append_only();

-- Row-level triggers do not fire for TRUNCATE, so without this the two above are
-- theatre: TRUNCATE would remove every row while satisfying both, and every
-- deferred balance check would pass over an empty table.
CREATE TRIGGER ledger_entries_no_truncate
    BEFORE TRUNCATE ON ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_assert_append_only();

CREATE TRIGGER ledger_transactions_no_update
    BEFORE UPDATE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_append_only();

CREATE TRIGGER ledger_transactions_no_delete
    BEFORE DELETE ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_assert_append_only();

CREATE TRIGGER ledger_transactions_no_truncate
    BEFORE TRUNCATE ON ledger_transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_assert_append_only();

-- =============================================================================
-- INDEXES
--
-- Derived from the reads that will actually exist, following the catalogue
-- migration's discipline: an index nothing queries is pure write amplification,
-- and ledger_entries is on the write path of every bet placed and settled.
--
-- Already indexed by constraints declared above, and therefore not repeated:
--   ledger_entries (transaction_id, entry_index)  PK -- also what the balance
--                                                 trigger's aggregate scans
--   ledger_transactions (id) PK, (id, kind, occurred_at) UNIQUE
--   wagers (id) PK
--   legs (id) PK, (wager_id, selection_id) UNIQUE, (wager_id, market_id) UNIQUE
--   round_robins (id) PK, (id, selection_count) UNIQUE, (id, stake_*) UNIQUE
--   round_robin_sizes (round_robin_id, size) PK
-- =============================================================================

-- THE BALANCE READ. CLAUDE.md §6's play-money balance, the bet slip's "can this
-- customer afford it", and 00005's period-scoped responsible-gaming limits are
-- all this one index.
--
-- Leading with the account so a balance is a contiguous range; occurred_at third
-- so a period-scoped sum is a range within it. INCLUDE carries the two payload
-- columns, which makes both the balance fold and the limit check INDEX-ONLY
-- scans -- no heap visit per entry. That is what makes the derived balance in
-- `account_balances` cheap enough to justify never materialising it.
CREATE INDEX ledger_entries_account_idx
    ON ledger_entries (account_kind, account_user_id, occurred_at)
    INCLUDE (amount_minor, kind);

-- "Show me the money movements of this wager" -- the settlement audit trail on
-- the wager-detail screen, and the referencing side of the wager_id FK so the
-- RESTRICT check is a lookup rather than a scan. Partial: a grant has no wager
-- and would only bloat it.
CREATE INDEX ledger_transactions_wager_idx
    ON ledger_transactions (wager_id)
    WHERE wager_id IS NOT NULL;

-- Wager history for one customer, newest first (CLAUDE.md §6).
CREATE INDEX wagers_user_placed_idx
    ON wagers (user_id, placed_at DESC);

-- OPEN POSITION TRACKING (§6) and the exposure metric. The predicate is
-- WagerStatus.HoldsEscrow() -- placed or open -- which ledger.go names as "the
-- predicate the exposure metric and the 'at risk' figure are built on". Partial
-- because settled tickets accumulate without bound and are never an open
-- position, so this index stays small forever while the table does not.
CREATE INDEX wagers_user_open_idx
    ON wagers (user_id)
    WHERE status IN ('placed', 'open');

-- The tickets of one round robin: the history screen groups them under their
-- parent, and this is the parent side of the composite FK's check.
CREATE INDEX wagers_round_robin_idx
    ON wagers (round_robin_id)
    WHERE round_robin_id IS NOT NULL;

-- THE SETTLEMENT READ. `settle` consumes a results feed and must find the
-- ungraded legs an event touches. Status is the trailing column rather than a
-- partial predicate so the same index also serves the events FK's RESTRICT check
-- and "everything ever bet on this event".
CREATE INDEX legs_event_status_idx
    ON legs (event_id, status);

-- A market being voided or settled has to reach the legs on it, and this is the
-- referencing side of the composite market FK. The UNIQUE constraints above lead
-- with wager_id and so cannot serve a market_id lookup.
CREATE INDEX legs_market_idx
    ON legs (market_id);

-- CLV (CLAUDE.md §6, phase 9): an event-time join of a leg's booked price
-- against its selection's CLOSING price. That join walks selection -> legs, which
-- neither UNIQUE constraint can serve. Also the referencing side of the
-- selections FK.
CREATE INDEX legs_selection_idx
    ON legs (selection_id);

-- A customer's round robins, newest first, and the users FK's RESTRICT check.
CREATE INDEX round_robins_user_idx
    ON round_robins (user_id, placed_at DESC);

-- DELIBERATELY ABSENT, recorded so each omission reads as a decision:
--
--   ledger_transactions (occurred_at)   Every real time-scoped question is
--                                       asked about an ACCOUNT ("this user's
--                                       grants this month", "the house's P&L
--                                       today") and is served by
--                                       ledger_entries_account_idx. A global
--                                       ledger-by-time scan is the integrity
--                                       audit, which reads every row anyway.
--   ledger_entries (kind)               Always paired with an account in
--                                       practice; `kind` rides in the INCLUDE
--                                       above, so the filter costs no heap visit.
--   legs (price_book_id)                Books are never deleted (the catalogue
--                                       migration's RESTRICT spine) and the
--                                       column has a handful of distinct values,
--                                       so it can serve no selective query.
--   wagers (status) alone               A global "all open wagers" is an
--                                       operator report, not a hot path; every
--                                       product query is scoped by user first.
--   legs (wager_id)                     Already the leading column of two UNIQUE
--                                       constraints.

-- =============================================================================
-- DERIVED VIEWS
--
-- Neither is a table and neither is materialised. They are named queries, so
-- they cannot drift from the rows they read -- which is the whole content of
-- CLAUDE.md §4's "Balances are derived, never stored as a mutable field."
-- =============================================================================

-- domain.Balances(), as SQL. One row per account that has ever been touched.
--
-- ledger.go on why a zero-balance account still appears: "Accounts with a net
-- movement of zero are still present, with a zero balance -- 'this account was
-- touched and nets to nothing' is a different fact from 'this account does not
-- exist', and the ledger is the wrong place to blur the two." A GROUP BY
-- reproduces that exactly: an account with entries appears whatever they sum to,
-- and an account with none does not appear at all.
--
-- The cast to BIGINT is deliberate rather than incidental. sum(bigint) is
-- numeric, which sqlc would map to a string-ish Go type and force the Go side to
-- parse a balance -- exactly the seam CLAUDE.md §12 puts money in int64 to avoid.
-- Every entry is bounded by domain.MaxSafeMoney (2^53-1), so reaching bigint's
-- range would take on the order of a thousand maximal entries on one account; a
-- ledger in that state is already broken and an error on the cast is the right
-- outcome.
CREATE VIEW account_balances AS
SELECT account_kind,
       account_user_id,
       sum(amount_minor)::BIGINT AS balance_minor,
       count(*)                  AS entry_count,
       min(occurred_at)          AS first_movement_at,
       max(occurred_at)          AS last_movement_at
  FROM ledger_entries
 GROUP BY account_kind, account_user_id;

COMMENT ON VIEW account_balances IS
    'domain.Balances() as a query: every account''s balance derived by folding ledger_entries. THE ONLY correct source for a balance. Not a table and not materialised -- a stored balance can be stale, and a bet slip validated against a stale balance is an overdraft.';

-- domain.LedgerIsBalanced(), as SQL: the audit hook that survives the trip
-- through the database, where the Go type system's guarantee no longer reaches.
--
-- A healthy ledger has total_minor = 0 AND unbalanced_transactions = 0. Both are
-- guaranteed by the deferred trigger above, which is exactly why the view is
-- worth having: it is the independent check that the guarantee is holding, and
-- the natural source for a Grafana panel and for an integration test's final
-- assertion. If it ever reports non-zero, a constraint has been disabled.
CREATE VIEW ledger_integrity AS
SELECT (SELECT coalesce(sum(amount_minor), 0) FROM ledger_entries)   AS total_minor,
       (SELECT count(*) FROM ledger_transactions)                    AS transaction_count,
       (SELECT count(*) FROM ledger_entries)                         AS entry_count,
       (SELECT count(*)
          FROM (SELECT transaction_id
                  FROM ledger_entries
                 GROUP BY transaction_id
                HAVING sum(amount_minor) <> 0 OR count(*) < 2) AS bad) AS unbalanced_transactions,
       (SELECT count(*)
          FROM ledger_transactions t
         WHERE NOT EXISTS (SELECT 1 FROM ledger_entries e
                            WHERE e.transaction_id = t.id))          AS empty_transactions;

COMMENT ON VIEW ledger_integrity IS
    'domain.LedgerIsBalanced() as a query. A healthy ledger reports total_minor = 0, unbalanced_transactions = 0 and empty_transactions = 0. A non-zero row means a constraint has been disabled.';

-- =============================================================================
-- INVARIANTS THIS SCHEMA DELIBERATELY DOES NOT ENFORCE, AND WHERE THEY LIVE
--
-- Recorded so each gap reads as a decision rather than an oversight, and so
-- phase 8 and phase 9 know what they own.
--
--   1. A stake transaction's amount equals its wager's stake; a settlement
--      transaction's customer credit equals wagers.returned_minor; and its kind
--      matches settlementEntryKind(status) -- won->payout, lost->loss,
--      void|push->refund, cashed_out->cash_out.
--
--      Enforceable as another deferred trigger, and deliberately not. The
--      zero-sum invariant is LOCAL to one transaction, so the database can check
--      it without knowing anything about betting. These are cross-aggregate
--      agreements between the ledger and the wager, and encoding them here would
--      put settlement policy -- including the partial-void repricing rule -- in
--      two places. They are Go-side invariants (NewStakeTransaction and
--      NewSettlementTransaction construct both sides from one Wager, so they
--      cannot disagree) plus a phase 9 reconciliation query, where a mismatch is
--      a report and not a rejected settlement.
--
--   2. The per-kind SIGN convention: a grant debits issuance and credits cash, a
--      stake debits cash and credits escrow. domain.transferEntries is "the only
--      place in the package that writes a two-sided movement by hand, so the sign
--      convention has exactly one home". A row-level CHECK cannot see the other
--      half of the pair, and a trigger that could would be a second home.
--
--   3. The number of tickets a round robin expands into equals
--      RoundRobin.CombinationCount(). See the note inside
--      wagers_assert_shape(): computing sums of binomial coefficients in plpgsql
--      would be a second implementation of Combinations().
--
--   4. potential_payout_minor = MulFloat(stake, accepted_decimal, rounding).
--      See the header on `wagers`: PostgreSQL cannot express two of the three
--      rounding modes, and a second implementation would reject legitimate bets.
--
--   5. That a leg's event_id and market_id agree with the catalogue's own
--      hierarchy -- that market_id really is selection_id's market, and event_id
--      really is that market's event. See the cross-agent note below: the keys
--      needed to pin them declaratively do not exist upstream, and this migration
--      does not own the file that would declare them.
--
--
-- CROSS-AGENT NOTE FOR THE CATALOGUE MIGRATION (NOT A CHANGE MADE HERE)
--
-- Two additional UNIQUE constraints upstream would let `legs` pin its
-- denormalised relationship keys the same way it already pins market_type:
--
--     ALTER TABLE selections ADD CONSTRAINT selections_id_market_key
--         UNIQUE (id, market_id);
--     ALTER TABLE markets    ADD CONSTRAINT markets_id_event_key
--         UNIQUE (id, event_id);
--
-- With those, legs could carry
--     FOREIGN KEY (selection_id, market_id) REFERENCES selections (id, market_id)
--     FOREIGN KEY (market_id, event_id)     REFERENCES markets (id, event_id)
-- and a leg claiming a selection from one market under another market's id -- or
-- a market under the wrong event -- would become unstorable rather than merely
-- wrong. Both are trivially satisfied (each pair leads with a primary key) and
-- cost one narrow index each. Reported rather than added: `migrations/00002` is
-- owned by another agent, and adding a constraint to another migration's table is
-- how concurrently authored schemas corrupt each other.
--
-- REQUIREMENT ON PHASE 8 (internal/betting), restated because it is a hard one:
-- the round-robin expansion MUST mint a DISTINCT domain.LegID per (ticket,
-- selection). RoundRobin.Combinations() returns subsets of the same []Leg values,
-- so passing them through verbatim produces repeated LegIDs and the second
-- INSERT violates legs' primary key. See the header on `legs`.
-- =============================================================================

-- +goose Down

-- Reversibility, per CLAUDE.md §12: "Migrations are forward-only, and every one
-- is reversible in review before it is applied." This Down is the exact inverse
-- of the Up, and it is proven to run -- it is the review artifact, not an
-- operational tool.
--
-- Read that distinction literally here. Running this Down against a database
-- holding real wagers DESTROYS THE LEDGER, and the append-only triggers cannot
-- stop it, because DROP TABLE is not a row operation. Balances are derived from
-- these rows, so a dropped table is not recoverable from anywhere else in the
-- system. Roll back on a throwaway database during review, never on one that has
-- served traffic.
--
-- Order: views first (they depend on the tables), then indexes, then tables in
-- reverse dependency order -- dropping a table drops its own triggers, including
-- its constraint triggers -- and only then the functions those triggers called.
-- Reversing the last two steps fails with a dependency error.

DROP VIEW IF EXISTS ledger_integrity;
DROP VIEW IF EXISTS account_balances;

DROP INDEX IF EXISTS round_robins_user_idx;
DROP INDEX IF EXISTS legs_selection_idx;
DROP INDEX IF EXISTS legs_market_idx;
DROP INDEX IF EXISTS legs_event_status_idx;
DROP INDEX IF EXISTS wagers_round_robin_idx;
DROP INDEX IF EXISTS wagers_user_open_idx;
DROP INDEX IF EXISTS wagers_user_placed_idx;
DROP INDEX IF EXISTS ledger_transactions_wager_idx;
DROP INDEX IF EXISTS ledger_entries_account_idx;

DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS legs;
DROP TABLE IF EXISTS wagers;
DROP TABLE IF EXISTS round_robin_sizes;
DROP TABLE IF EXISTS round_robins;

DROP FUNCTION IF EXISTS ledger_assert_append_only();
DROP FUNCTION IF EXISTS ledger_assert_transaction_balanced();
DROP FUNCTION IF EXISTS betting_reject_truncate();
DROP FUNCTION IF EXISTS wagers_assert_shape();
DROP FUNCTION IF EXISTS legs_assert_transition();
DROP FUNCTION IF EXISTS wagers_assert_transition();
DROP FUNCTION IF EXISTS betting_set_updated_at();
