-- =============================================================================
-- 00001  extensions and enums  (the bootstrap migration)
-- =============================================================================
--
-- First migration in the set. Nothing in migrations/ may assume anything this
-- file does not establish, and this file may assume nothing but a running
-- PostgreSQL 17 server built from `timescale/timescaledb:latest-pg17`
-- (CLAUDE.md §3, §9).
--
-- It does exactly two things:
--
--   1. Creates the two PostgreSQL extensions the rest of the schema and the
--      observability stack require, with preconditions and postconditions
--      asserted loudly (CLAUDE.md §12: "fail fast and loudly on a bad config",
--      applied to schema prerequisites the way 00007_platform applies it).
--
--   2. Records, in one place, the AUTHORITATIVE CATALOGUE OF EVERY ENUM VALUE
--      the schema is allowed to store, extracted from `internal/domain` by
--      reading the `String()` switch bodies rather than by inferring from
--      constant identifiers. See "ENUM CATALOGUE" below. Every sibling
--      migration copies its value lists from that catalogue verbatim.
--
-- It deliberately creates NO TABLE, NO TYPE, NO FUNCTION and NO ROW. The reason
-- for each of those absences is stated below; none of them is an oversight.
--
--
-- WHY THERE ARE NO `CREATE TYPE ... AS ENUM` STATEMENTS IN A FILE CALLED
-- "extensions_and_enums"
-- ----------------------------------------------------------------------------
-- Because the frozen enum representation for this schema is TEXT + a named
-- CHECK constraint, and a CHECK constraint is a property of a COLUMN, not a
-- standalone object. There is therefore nothing for the bootstrap migration to
-- create: the enum values are enforced on the tables that hold them, in the
-- migrations that create those tables.
--
-- The choice was already made and committed to on disk before this file was
-- written, in both directions of the schema:
--
--   * 00005_accounts_and_auth.sql, "ENUM REPRESENTATION: TEXT + CHECK,
--     EVERYWHERE" -- users.status, refresh_token_families.revoked_reason,
--     user_limits.kind, user_limits.period are all
--     `TEXT ... CONSTRAINT <name> CHECK (col IN (...))`.
--   * 00007_platform.sql, "ENUM REPRESENTATION: TEXT + CHECK, NOT NATIVE ENUM
--     TYPES" -- audit_log.actor_kind, audit_log.outcome, likewise.
--
-- This file MATCHES that, because consistency across one schema outranks any
-- argument about which representation is nicer. The rationale, and the honest
-- cost of the choice, both stated once here so the whole set can point at it:
--
--   FOR TEXT + CHECK
--   ~~~~~~~~~~~~~~~~
--   * Reversibility, which is the deciding argument. CLAUDE.md §12: "Migrations
--     are forward-only, and every one is reversible in review before it is
--     applied." Widening or narrowing a CHECK is `ALTER TABLE ... DROP
--     CONSTRAINT` + `ADD CONSTRAINT`; both run inside the migration's
--     transaction and each has an exact one-line inverse to write in the Down
--     block. A native ENUM has no `DROP VALUE` at all, so retiring a value
--     means creating a replacement type, rewriting every column that uses it,
--     and dropping the old one -- a migration whose Down cannot be reviewed the
--     way §12 requires.
--   * Transaction safety. goose wraps each migration in a transaction, and
--     `ALTER TYPE ... ADD VALUE` has a sharp edge inside one: the new label is
--     not usable by other statements in the same transaction. A CHECK has no
--     such rule.
--   * One Go type per concept. `internal/domain` already owns every value set
--     here and already serialises it as lowercase text through
--     `String()`/`MarshalText()` with an exact `Parse*` inverse, so TEXT is the
--     identity mapping: no pgx type registration, no second sqlc-generated Go
--     type for a set the domain already models, nothing to drift.
--   * Legible in `psql` without joining `pg_enum`, which matters when the
--     double-entry ledger is being audited by hand.
--
--   AGAINST TEXT + CHECK -- the cost, paid knowingly
--   ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
--   * No type-level guarantee. A native ENUM makes the value set a property of
--     the TYPE, so a new column of that type is correct by construction and a
--     typo is a parse error at DDL time. With TEXT + CHECK the value set is
--     re-stated per column, so every new column is a fresh opportunity to
--     mis-copy a spelling, and the mistake is caught only when a row is
--     rejected -- or, if the typo is in the CHECK rather than in the
--     application, never.
--   * The value list is duplicated once per column that holds it. Six tables
--     holding a status is six literal lists to keep in step.
--   * Storage: a short TEXT costs ~1 byte of header plus the string, against an
--     ENUM's fixed 4. Noise at this scale, but it is not free.
--   * Ordering: an ENUM sorts in declaration order, so `ORDER BY status` can be
--     lifecycle order for free. TEXT sorts alphabetically, so any lifecycle
--     ordering needs an explicit CASE.
--
-- The first cost is the dangerous one, and it is exactly why the catalogue
-- below exists and why it lives in the migration that runs first. It is not
-- documentation for its own sake; it is the mitigation for the one real defect
-- of the chosen representation.
--
--
-- WHY THERE IS NO SHARED `set_updated_at()` TRIGGER FUNCTION HERE
-- ----------------------------------------------------------------------------
-- 00007_platform.sql invites one: it namespaces its own function
-- `platform_set_updated_at()` and notes "If a shared `set_updated_at()` is
-- later introduced in the bootstrap migration, this one collapses into it in a
-- follow-up migration."
--
-- The invitation is declined, and the reason it is declined has changed since
-- this block was first written -- read the history, because the first reason no
-- longer applies and only the second one is doing the work now.
--
-- ORIGINALLY, the first reason was a live disagreement: 00005 claimed "NO
-- updated_at TRIGGER, ANYWHERE IN THIS SCHEMA", on the ground that the domain's
-- state transitions take the instant as an explicit parameter
-- (`Wager.Settle(status, amount, at)`, `Leg.WithStatus(status, at)`) precisely
-- so that a redelivered Kafka message re-applies the ORIGINAL instant rather
-- than the wall clock, and a trigger stamping `now()` would silently discard
-- the value the domain worked to preserve.
--
-- THAT DISAGREEMENT IS SETTLED. It was settled in 00005's favour on the
-- substance and against it on the mechanism: no trigger anywhere in this schema
-- writes a DOMAIN instant, and 00002's resolution -- split the provider instant
-- into its own `observed_at` column and trigger only the row-bookkeeping
-- `updated_at` -- is now the schema-wide convention. 00002, 00005, 00006 and
-- 00007 each install `updated_at` triggers on their own mutable tables through
-- their own namespaced function (`catalogue_`, `auth_`, `betting_`,
-- `platform_`). The redelivery argument survives intact and still forbids the
-- thing it was raised to forbid.
--
-- The remaining reason, which was always independently sufficient: a shared
-- function created here would be used by whichever sibling migrations happened
-- to know about it. A function
-- that some tables' triggers depend on and others do not is precisely the
-- cross-migration coupling 00007 warns about -- its Down would be unable to
-- drop it, and this file's Down would be able to orphan every trigger that
-- referenced it. Four two-line functions with four independent Downs are
-- cheaper than one function four Downs must coordinate around. Collapsing them
-- is a deliberate follow-up migration if it is ever worth doing, not a
-- bootstrap side effect.
--
--
-- WHY THERE ARE NO ROWS
-- ----------------------------------------------------------------------------
-- Not one. Every value a user sees must have travelled
-- `provider -> ingest -> Kafka -> normalizer -> pricer -> Postgres`. An empty
-- database after `make up` is CORRECT; data arrives from ingest in phase 3.
--
-- Nor is there a lookup TABLE for any of the value sets below. A reference row
-- is only justified where the closed set has to be joined to or referenced by a
-- foreign key; here the sets are enforced by CHECK, so there is nothing to seed
-- by construction. `Sport` and `League` are the near-miss worth naming: they
-- look like closed sets but `internal/domain/sport.go` models them as ENTITIES
-- on purpose -- "the set of sports is provider data" -- so they are ingested
-- rows in the catalogue migration, never a seeded enumeration here.
--
--
-- =============================================================================
-- ENUM CATALOGUE -- AUTHORITATIVE. COPY FROM HERE; DO NOT RE-DERIVE.
-- =============================================================================
--
-- Every value below was read out of the `String()` switch body named in the
-- citation, not inferred from the Go constant identifier. The distinction is
-- load-bearing: `EventStatusLive` emits "live", `EntryKindCashOut` emits
-- "cash_out", `WagerStatusCashedOut` emits "cashed_out", `AccountKindUserCash`
-- emits "user_cash". Guessing any of those from the identifier gives a string
-- the application cannot read back, and phase 3 is the first time real data
-- would reveal it.
--
-- `MarshalText()` on each of these types delegates to `String()` after
-- rejecting the invalid zero value, and each `Parse*` is an exact,
-- case-sensitive inverse with no aliases. So the set of strings that can reach
-- the database is exactly the set below.
--
-- EVERY TYPE HAS AN INVALID ZERO VALUE (`...Unknown`) THAT EMITS "unknown".
-- "unknown" IS NEVER A STORABLE VALUE. `MarshalText` returns an error for it
-- (`if !x.Valid() { return nil, ... }`), so no CHECK list in this schema may
-- contain it. A column that needs "not yet known" is NULL, not "unknown".
--
-- ---------------------------------------------------------------------------
-- Catalogue / market tree                            (migration 00002 et seq.)
-- ---------------------------------------------------------------------------
--   book_kind          internal/domain/book.go:40        BookKind.String()
--       'external', 'synthetic'
--
--   event_kind         internal/domain/event.go:45       EventKind.String()
--       'match', 'outright'
--
--   event_status       internal/domain/event.go:158      EventStatus.String()
--       'scheduled', 'live', 'suspended', 'ended', 'settled', 'postponed',
--       'cancelled'
--
--   market_type        internal/domain/market.go:201     MarketType.String()
--       'moneyline', 'spread', 'total', 'player_prop', 'futures'
--
--   market_status      internal/domain/market.go:353     MarketStatus.String()
--       'open', 'suspended', 'closed', 'settled', 'voided'
--
--   selection_role     internal/domain/selection.go:45   SelectionRole.String()
--       'home', 'away', 'draw', 'over', 'under', 'outright'
--
-- ---------------------------------------------------------------------------
-- Wagering and ledger                                (migration 00006 et seq.)
-- ---------------------------------------------------------------------------
--   wager_kind         internal/domain/wager.go:217      WagerKind.String()
--       'straight', 'parlay', 'round_robin', 'teaser'
--
--   wager_status       internal/domain/wager.go:357      WagerStatus.String()
--       'placed', 'open', 'won', 'lost', 'void', 'push', 'cashed_out'
--
--   leg_status         internal/domain/leg.go:104        LegStatus.String()
--       'pending', 'won', 'lost', 'void', 'push'
--
--   account_kind       internal/domain/ledger.go:131     AccountKind.String()
--       'user_cash', 'user_escrow', 'house', 'issuance'
--
--   entry_kind         internal/domain/ledger.go:337     EntryKind.String()
--       'grant', 'stake', 'payout', 'loss', 'refund', 'cash_out', 'adjustment'
--
-- ---------------------------------------------------------------------------
-- Pricing                                            (migration 00004 et seq.)
-- ---------------------------------------------------------------------------
--   devig_method       internal/domain/odds/devig.go:202 DevigMethod.String()
--       'multiplicative', 'additive', 'power', 'shin'
--
--   attribution        internal/domain/odds/vig.go:380   Attribution.String()
--       'proportional', 'uniform'
--
--       Note: `Attribution` has NO valid default in the domain -- its zero
--       value is invalid so that an unset value fails loudly. A column holding
--       it must therefore be NOT NULL with no DEFAULT, or nullable; never
--       `DEFAULT 'proportional'`.
--
-- ---------------------------------------------------------------------------
-- Types that are enums in Go but are NOT stored as their own columns
--
-- One entry below (`Rounding`) has a single justified exception, recorded here
-- rather than left as a contradiction for a future reader to trip over. This
-- section is otherwise a prohibition.
-- ---------------------------------------------------------------------------
--   LineRule           internal/domain/market.go:154
--       'forbidden', 'required', 'optional'
--       DERIVED, NOT STORED. It is the return value of
--       `MarketType.LineRule()` (market.go:265) -- a total function of the
--       market type. Persisting it creates a second, independently-writable
--       copy of a fact the market type already determines, and the two can
--       disagree. The `markets` table constrains the presence of its line
--       against its type directly instead.
--
--   Rounding           internal/domain/money.go:92
--       'half_away_from_zero', 'half_to_even', 'toward_zero'
--       GENERALLY a PARAMETER TO A CALCULATION rather than an attribute of
--       anything: it selects how one arithmetic result is rounded to minor
--       units, and most results that need one do not outlive the call.
--
--       ONE EXCEPTION, AND IT IS STORED: `wagers.rounding` (00006). This entry
--       originally read "Nothing in the system is 'a row with a rounding
--       mode'", which is wrong about exactly one row. `domain.Wager` holds a
--       `rounding Rounding` field (wager.go:516) and exposes it
--       (`Wager.Rounding()`, wager.go:813), because a partially-voided parlay
--       must reprice under the rule the ticket was WRITTEN under rather than
--       one picked fresh at settlement -- and money.go warns that "a silent
--       default is how a house edge appears in a ledger that nobody meant to
--       put one in". `NewWager` refuses the invalid zero value, so a wager
--       rehydrated from these rows without the column cannot be constructed at
--       all, and the stored `potential_payout_minor` would be unreproducible.
--
--       So a WAGER genuinely is "a row with a rounding mode". 00006 documents
--       the same conclusion at length under "ONE DISAGREEMENT WITH 00001'S
--       CATALOGUE" and flagged it rather than editing this file, which was not
--       its to change. Reconciled here by the phase-2 gate, which owns both.
--       The general rule still stands: a rounding mode is stored only where a
--       later recomputation must reuse it, never as decoration.
--
--   Format             internal/domain/odds/format.go:49
--       'american', 'decimal', 'fractional'
--       A PRESENTATION FORMAT. CLAUDE.md §7 defines `Decimal` as the canonical
--       price type -- "American and Fractional are DISPLAY formats: convert at
--       the edge, render, discard" -- so no price column is ever denominated in
--       a Format. The one legitimate future home is a per-user display
--       preference for CLAUDE.md §6's "odds format toggle"; 00005 did not add
--       such a column, so today this set has no place in the schema at all.
--
-- ---------------------------------------------------------------------------
-- Value sets frozen by 00005/00007 that have NO domain constant yet
-- ---------------------------------------------------------------------------
-- Listed for completeness so this catalogue is the whole picture. These are NOT
-- owned here -- the file that creates the column owns the list -- and they are
-- the only value sets in the schema without a Go counterpart, because
-- `internal/domain` deliberately models no user entity and no audit record.
-- Phase 5 (`internal/auth`) and `internal/audit` must define constants emitting
-- exactly these spellings.
--
--   users.status                            00005
--       'active', 'suspended', 'self_excluded', 'closed'
--   refresh_token_families.revoked_reason    00005
--       'logout', 'reuse_detected', 'credential_change', 'operator'
--   user_limits.kind                         00005
--       'grant', 'stake', 'loss', 'session'
--       -- three of the four are deliberately identical to EntryKind spellings
--          so enforcing a limit is a sum over ledger_entries on the same string
--   user_limits.period                       00005
--       'day', 'week', 'month', 'session'
--   audit_log.actor_kind                     00007
--       'user', 'admin', 'system'
--   audit_log.outcome                        00007
--       'success', 'failure'
--
-- =============================================================================

-- +goose Up

-- ---------------------------------------------------------------------------
-- Preconditions.
--
-- Checked before anything is created so that a wrong server produces one
-- actionable sentence rather than a cascade of confusing failures three
-- migrations later.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $precondition$
DECLARE
    v_num int := current_setting('server_version_num')::int;
BEGIN
    -- CLAUDE.md §3 names "Postgres 17 + TimescaleDB". The floor is asserted
    -- rather than assumed because this schema uses PostgreSQL features that a
    -- 12/13-era server would accept with different semantics, and because
    -- `gen_random_uuid()` is only a CORE function from 13 onward (see the note
    -- on pgcrypto below).
    IF v_num < 170000 THEN
        RAISE EXCEPTION
            'sharpline requires PostgreSQL 17 or newer; this server is %',
            current_setting('server_version')
            USING HINT = 'CLAUDE.md section 3 pins timescale/timescaledb:latest-pg17. '
                         'Point GOOSE_DBSTRING at that container, not at a host PostgreSQL.';
    END IF;
END
$precondition$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- timescaledb
--
-- CLAUDE.md §3: "Postgres 17 + TimescaleDB -- relational core plus hypertables
-- for the odds time-series." CLAUDE.md §4 on Price: "Immutable; a new price is
-- a new row. This is the hypertable." 00007_platform.sql makes `audit_log` a
-- second hypertable and asserts in its own preflight that "the bootstrap
-- migration must run CREATE EXTENSION timescaledb before this one" -- this is
-- that statement.
--
-- IF NOT EXISTS IS LOAD-BEARING, NOT DEFENSIVE HABIT.
-- The extension is ALREADY INSTALLED before any migration runs. The image's own
-- init hook, /docker-entrypoint-initdb.d/000_install_timescaledb.sh, executes
-- `CREATE EXTENSION IF NOT EXISTS timescaledb CASCADE` against `postgres`,
-- `template1`, AND ${POSTGRES_DB} at initdb time. Because it lands in
-- `template1`, every database created afterwards inherits it too.
--
-- Verified on this exact image (timescale/timescaledb:latest-pg17, digest
-- ...566fd6, PostgreSQL 17.10, timescaledb 2.29.1): on a freshly initialised
-- container a bare `CREATE EXTENSION timescaledb;` fails with
-- `ERROR: extension "timescaledb" already exists`. Without IF NOT EXISTS this
-- migration would abort on every clean database -- including the throwaway one
-- `make migrate-dry-run` builds in CI.
--
-- The statement is kept anyway rather than deleted as redundant, because the
-- schema must state its own prerequisites. A database restored from a plain
-- `pg_dump` into a server whose `template1` lacks the extension gets it here.
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- ---------------------------------------------------------------------------
-- pg_stat_statements
--
-- CLAUDE.md §9 makes observability a first-class deliverable and names odds
-- staleness the headline SLO; `deploy/postgres/postgresql.conf` already
-- preloads this extension for it
-- (`shared_preload_libraries = 'timescaledb,pg_stat_statements'`, plus
-- `pg_stat_statements.max`, `.track`, `.track_utility` and
-- `track_io_timing = on` tuned around it).
--
-- Preloading alone does not create the view. Without a `CREATE EXTENSION` in
-- some container-run path, every one of those settings is dead weight and the
-- Grafana dashboard has no query-level source. Migrations are the only
-- container-run path that touches this database -- compose mounts no
-- `/docker-entrypoint-initdb.d` script, and Terraform owns Kafka topics and
-- Grafana dashboards, not PostgreSQL extensions -- so this is where it belongs.
--
-- Two honest costs, both accepted:
--
--   1. Rolling this migration back drops the extension, which discards
--      accumulated query statistics. That is a real operational side effect of
--      a schema rollback. It is accepted because the alternative -- a Down that
--      leaves behind an object the Up created -- is the worse failure, and
--      because the statistics are derived observability data, never a source of
--      truth (nothing reads them but a dashboard).
--
--   2. `CREATE EXTENSION` succeeds even when the library is NOT preloaded, but
--      selecting from the view then fails with `pg_stat_statements must be
--      loaded via "shared_preload_libraries"`. Verified: the throwaway database
--      in `make migrate-dry-run` is started WITHOUT the mounted conf, so its
--      `shared_preload_libraries` is `timescaledb` only, the CREATE succeeds,
--      and the view is unusable there. That is the correct behaviour for a
--      throwaway database that no dashboard scrapes, and it is why the check
--      below is a WARNING rather than an EXCEPTION -- making it fatal would
--      break CI to enforce a setting CI has no reason to carry.
--
-- Deliberately NOT created, because nothing uses them (verified by reading
-- 00005 and 00007, the two migrations already on disk):
--
--   * pgcrypto -- 00007 calls `gen_random_uuid()` for `audit_log.id` and
--     `market_suspensions.id`. That function has been a CORE PostgreSQL
--     function since 13 and is NOT owned by any extension here; verified on
--     this image, `SELECT gen_random_uuid()` works with zero extensions beyond
--     timescaledb, and `pg_depend` shows the function belongs to no extension.
--     Creating pgcrypto for it would add a dependency that grants nothing.
--   * citext -- 00005 considered and explicitly rejected it for `users.email`
--     ("`citext` needs a non-core extension for one column and hides a
--     locale-dependent comparison behind an `=` that looks ordinary"),
--     normalising to lowercase on the way in with a CHECK instead.
--   * pg_trgm / btree_gist / unaccent -- available on this image, used by
--     nothing on disk. An unused extension is schema surface with a CVE feed
--     attached.
-- ---------------------------------------------------------------------------
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

-- ---------------------------------------------------------------------------
-- Postconditions.
--
-- Asserts the state the rest of the migration set is entitled to assume, so a
-- failure is attributed here instead of surfacing as
-- `function create_hypertable(...) does not exist` in migration 00003.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $postcondition$
DECLARE
    v_ts_version  text;
    v_ts_major    int;
    v_preloaded   text := current_setting('shared_preload_libraries', true);
BEGIN
    SELECT extversion INTO v_ts_version
      FROM pg_extension
     WHERE extname = 'timescaledb';

    IF v_ts_version IS NULL THEN
        RAISE EXCEPTION
            'the timescaledb extension is absent after CREATE EXTENSION succeeded'
            USING HINT = 'timescaledb must be listed in shared_preload_libraries; it refuses '
                         'to be created otherwise. See deploy/postgres/postgresql.conf.';
    END IF;

    -- The 2.x line is required, not merely preferred: `create_hypertable` took
    -- its current form in 2.x, and both hypertables in this schema (prices in
    -- 00003, audit_log in 00007) are written against it.
    v_ts_major := split_part(v_ts_version, '.', 1)::int;
    IF v_ts_major < 2 THEN
        RAISE EXCEPTION
            'sharpline requires TimescaleDB 2.x or newer; this server has %', v_ts_version
            USING HINT = 'The hypertable definitions in this schema use the 2.x create_hypertable API.';
    END IF;

    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements') THEN
        RAISE EXCEPTION
            'the pg_stat_statements extension is absent after CREATE EXTENSION succeeded';
    END IF;

    -- Non-fatal on purpose: see cost (2) in the pg_stat_statements block above.
    IF v_preloaded IS NULL OR position('pg_stat_statements' in v_preloaded) = 0 THEN
        RAISE WARNING
            'pg_stat_statements is installed but NOT in shared_preload_libraries (%), so its view will error on read',
            coalesce(v_preloaded, '<unset>')
            USING HINT = 'Expected for the throwaway database in `make migrate-dry-run`. '
                         'For a real deployment, deploy/postgres/postgresql.conf must be mounted.';
    END IF;

    RAISE NOTICE 'sharpline bootstrap: PostgreSQL %, timescaledb %',
        current_setting('server_version'), v_ts_version;
END
$postcondition$;
-- +goose StatementEnd

-- +goose Down

-- ---------------------------------------------------------------------------
-- Drops what this migration created, and nothing else.
--
-- pg_stat_statements WAS created here -- verified absent on a freshly
-- initialised container -- so dropping it is the exact inverse.
--
-- timescaledb IS DELIBERATELY NOT DROPPED, and this is the one asymmetry in
-- this file. It is not an omission and `DROP EXTENSION IF EXISTS timescaledb`
-- must not be added.
--
-- The extension pre-exists this migration in every environment this project
-- runs in. That is a guarantee, not an assumption, on two independent grounds:
--
--   * The image creates it at initdb, into `template1` among others (see the
--     verified note in the Up block), so every database on this image has it
--     before goose connects.
--   * That image is the only PostgreSQL this project ever runs against.
--     CLAUDE.md §9: "Every workload in the cluster, stateful ones included --
--     Postgres, Redis, and Kafka run as StatefulSets with PVCs, not as external
--     managed services." There is no managed-PostgreSQL path where the
--     extension might be missing.
--
-- So the pre-migration baseline INCLUDES timescaledb, and `DROP EXTENSION`
-- would not reverse the Up -- it would carry the database to a state BEHIND
-- its own starting point, destroying an object this migration did not create.
-- A Down that destroys what the Up did not create is not a reversal; it is a
-- second forward migration pointed backwards, and an unreviewed one.
--
-- It would also be actively dangerous. `DROP EXTENSION timescaledb` cascades
-- to every hypertable, chunk, continuous aggregate and background job built on
-- it. Ordinarily `down-to 0` has already removed those, but a partially
-- applied rollback, a hand-run `goose down` at the wrong version, or a future
-- migration that adds a hypertable without a matching Down would each turn
-- this one line into silent, unrecoverable data loss on prices and audit_log.
-- The blast radius is enormous and the benefit is zero.
--
-- Reversibility of THIS migration is therefore complete as written: after the
-- Down, the database is byte-for-byte in the state the image produced. Verified
-- by `up -> down-to 0 -> up` against a real server.
-- ---------------------------------------------------------------------------
DROP EXTENSION IF EXISTS pg_stat_statements;
