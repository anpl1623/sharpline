-- =============================================================================
-- 00002  catalogue
-- =============================================================================
--
-- CLAUDE.md §4 defines the core language of the system in one line:
--
--     Sport → League → Event → Market → Selection → Price
--
-- This migration owns the first five. `Price` is NOT here: CLAUDE.md §4 says of
-- it "Immutable; a new price is a new row. This is the hypertable", and a
-- hypertable of immutable observations has nothing in common with a mutable
-- catalogue row. It gets its own migration.
--
-- `books` is also here. It is not a step in the hierarchy above — it is the
-- other axis a Price is keyed on ("odds for a selection at a book at an
-- instant") — but it is catalogue-shaped reference data with the same lifecycle
-- as the rest of this file, so it lives here rather than alone.
--
--
-- EVERY COLUMN CORRESPONDS TO A DOMAIN FIELD
-- ------------------------------------------
-- The tables below are a faithful projection of `internal/domain`: sport.go,
-- league.go, event.go, market.go, selection.go, book.go. A field with no column
-- would be data the system computes and then loses; a column with no field
-- would be data nothing in the domain can produce. The exceptions are named and
-- justified where they appear, and there are exactly four:
--
--   1. created_at / updated_at on every table — row bookkeeping mandated by the
--      project's schema conventions. The domain deliberately models no row
--      lifecycle at all.
--   2. events.observed_at / markets.observed_at — these DO have a domain
--      counterpart (Event.UpdatedAt(), Market.UpdatedAt()) but not the obvious
--      one. See the note on `observed_at` below; the name differs from the
--      domain's on purpose.
--   3. selections.market_type — a denormalised copy of markets.type, held
--      honest by a composite foreign key, which is the only way PostgreSQL can
--      enforce MarketType.AllowsRole() declaratively. See `selections`.
--   4. books.is_reference — this IS a domain field (Book.IsReference()); noted
--      here only because the "exactly one reference book" invariant that
--      book.go explicitly delegates to storage is enforced by an index in this
--      file and nowhere else.
--
--
-- NO SEED DATA. NOT ONE ROW. INCLUDING THE SYNTHETIC BOOK.
-- --------------------------------------------------------
-- CLAUDE.md's data flow is `provider → ingest → Kafka → normalizer → pricer →
-- Postgres`, and every value a user sees must have travelled it. An empty
-- catalogue after `make up` is CORRECT; rows arrive from ingest in phase 3.
--
-- The one row that looked like a legitimate exception is the synthetic in-house
-- book. CLAUDE.md §4 requires it ("Includes a synthetic in-house book for
-- development"), `domain.SyntheticBookSlug` is a compile-time constant the code
-- switches on, and `domain.NewSyntheticBook` fixes its slug, name and kind. So
-- three of its five columns come from constants, and inserting it would look
-- structural rather than sample.
--
-- It is NOT inserted, for two reasons that are about the other two columns:
--
--   * `id` is not a constant. NewSyntheticBook's own doc comment says "The
--     identifier is supplied by the caller because it comes from persistence".
--     A migration inserting this row would have to MINT an identifier — a value
--     with no domain counterpart, invented in SQL, that the Go code would then
--     have to hard-code to find again.
--
--   * `is_reference` is a RUNTIME decision, not a schema fact. book.go says
--     marking the synthetic book as the sharp reference is permitted precisely
--     so the offline no-API-key path has a reference at all — and whether that
--     path is active depends on whether `ODDS_API_KEY` is set, which a
--     migration cannot know. A migration that guesses it is a migration that is
--     wrong half the time, silently, in the +EV surface.
--
-- A migration that invents an identifier and guesses a runtime flag is seed
-- data wearing a schema costume. So `ingest` creates this row at startup
-- through `domain.NewSyntheticBook`, which is where the constant lives and
-- where the reference decision is made.
--
-- What this schema contributes instead is the two guarantees that make that
-- write safe no matter who performs it: `books.slug` is UNIQUE, so the write is
-- an idempotent upsert on a stable key rather than a duplicate-producing
-- insert; and `books_reference_unique_idx` enforces the "at most one reference
-- book" invariant that book.go hands to storage in as many words.
--
--
-- ENUM REPRESENTATION: TEXT + CHECK  (matched, not chosen)
-- -------------------------------------------------------
-- Every closed value set here is `TEXT` constrained by a named CHECK, never a
-- PostgreSQL native ENUM type.
--
-- This was not this migration's decision to make. 00005_accounts_and_auth.sql
-- and 00007_platform.sql were already on disk when this file was written and
-- both had already committed to TEXT + CHECK, each stating the same three
-- reasons at length: a CHECK is reversible where `ALTER TYPE ... ADD VALUE` has
-- no inverse; `ALTER TYPE ... ADD VALUE` has sharp edges inside the transaction
-- goose wraps each migration in; and a native ENUM makes sqlc emit a second Go
-- type for a closed set `internal/domain` already owns. Consistency across the
-- directory would justify matching even if the reasoning were weaker than it
-- is.
--
-- Every value spelled in a CHECK below is the exact output of the
-- corresponding domain type's String() method, and is accepted by its Parse
-- function. They are not re-derived here:
--
--   events.kind        domain.EventKind      match | outright
--   events.status      domain.EventStatus    scheduled | live | suspended
--                                            | ended | settled | postponed
--                                            | cancelled
--   markets.type       domain.MarketType     moneyline | spread | total
--                                            | player_prop | futures
--   markets.status     domain.MarketStatus   open | suspended | closed
--                                            | settled | voided
--   selections.role    domain.SelectionRole  home | away | draw | over
--                                            | under | outright
--   books.kind         domain.BookKind       external | synthetic
--
-- Unlike the four value sets 00005 had to mint ahead of their Go constants,
-- every value above already has a constant in `internal/domain` today.
--
--
-- WHY `observed_at` AND `updated_at` ARE BOTH PRESENT ON events AND markets
-- ------------------------------------------------------------------------
-- The domain's `Event.UpdatedAt()` is NOT a row-modification timestamp. Its own
-- doc comment is explicit: "It is the monotonicity guard for out-of-order bus
-- delivery, so it must be the provider's or ingester's observation time, never
-- a display time." Kafka delivers at least once and across partitions out of
-- order (CLAUDE.md §3), and `Event.stamp()` refuses an update stamped before
-- the one the value already carries.
--
-- Collapsing that into the conventional `updated_at` would put two different
-- meanings in one column and guarantee that someone eventually stamps it with
-- now(). So the provider instant is `observed_at` — named for what it is — and
-- `updated_at` stays what it is everywhere else in this schema: when the row
-- last changed.
--
-- 00005 raised exactly this objection ("A trigger overwriting that with now()
-- would silently discard the value the domain worked to preserve. The database
-- is not the clock here."). That objection is answered by the column split, not
-- ignored: `catalogue_set_updated_at()` below stamps `updated_at` and never
-- touches `observed_at`, so no domain value is discarded. 00007 established the
-- same trigger pattern in this directory, and this file follows it with the same
-- `_`-prefixed namespacing 00007 asks for.
--
-- THIS IS NOW THE SETTLED SCHEMA-WIDE CONVENTION, and 00005 states it as the
-- checkable invariant it always should have been: a trigger may stamp
-- `updated_at`; no trigger may write a column carrying a domain instant. 00005
-- installs its own `auth_set_updated_at()` on the same terms, 00006 adopted the
-- resolution explicitly, and 00001 records why the four namespaced functions
-- were not collapsed into one shared one.
--
-- PHASE 3, THIS IS YOUR CONTRACT: the ingest writer must guard `observed_at`
-- itself, because a CHECK constraint cannot compare NEW to OLD:
--
--     INSERT INTO events (...) VALUES (...)
--     ON CONFLICT (id) DO UPDATE SET ...
--     WHERE excluded.observed_at >= events.observed_at;
--
-- The WHERE clause is what makes an out-of-order redelivery a no-op instead of
-- a resurrection of an earlier state. It is deliberately NOT a trigger: a
-- trigger that RAISEd would turn routine at-least-once redelivery into an
-- error, which is the failure event.go's "s → s is legal" comment exists to
-- prevent, and a trigger that silently returned NULL would make `UPDATE 0`
-- indistinguishable from a lost write.
--
--
-- MONEY, ODDS, AND TIMES
-- ----------------------
-- There is no money on any table in this file. Nothing in the catalogue is
-- denominated in currency — stakes and balances live in the ledger — so
-- CLAUDE.md §12's "all money and stake values are integer minor units" has no
-- BIGINT to claim here. If a column on these tables ever does hold money it is
-- BIGINT minor units, never NUMERIC and never DOUBLE PRECISION.
--
-- `markets.line` is DOUBLE PRECISION, per §12: lines, odds and probabilities
-- are floats; only money is not.
--
-- Every timestamp is TIMESTAMPTZ, never TIMESTAMP. Phase 12's Flink event-time
-- joins depend on it, and the domain normalises every instant to UTC on
-- construction.
--
--
-- WHAT THIS MIGRATION DEPENDS ON: NOTHING
-- ---------------------------------------
-- No extension, no other table, no function. It is the root of the schema's
-- dependency graph and can be applied against an empty database.
--
-- In particular it does NOT require `timescaledb`, and it deliberately does not
-- require `pg_trgm` — see the search indexes at the bottom for what that costs
-- and why the cost was accepted rather than reaching outside this file.
--
-- What depends on IT, and must therefore be numbered above 00002:
--
--   * the prices hypertable — FKs to selections(id) and books(id)
--   * 00007_platform.sql — market_suspensions FKs to markets(id), and its
--     preflight asserts `to_regclass('public.markets') IS NOT NULL` with the
--     hint "The catalogue migration must be numbered lower than 00007". It is.

-- +goose Up

-- -----------------------------------------------------------------------------
-- Shared trigger function: maintain updated_at on this migration's tables.
--
-- Namespaced `catalogue_` on 00007's explicit advice: a bare
-- `set_updated_at()` is the obvious name and therefore the name every
-- concurrently-authored migration reaches for, and the first Down to run would
-- drop a function another migration's triggers still depend on. The prefix
-- makes the blast radius of this file's Down exactly this file.
--
-- It stamps `updated_at` and nothing else. It must never be extended to touch
-- `observed_at` — that column carries a provider observation instant, and the
-- database is not the clock for it.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION catalogue_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $catalogue_set_updated_at$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$catalogue_set_updated_at$;
-- +goose StatementEnd

COMMENT ON FUNCTION catalogue_set_updated_at() IS
    'BEFORE UPDATE trigger: stamps updated_at from the server clock. Never touches observed_at, which is a provider observation instant. Namespaced to this migration so its Down cannot orphan another migration''s triggers.';

-- -----------------------------------------------------------------------------
-- sports
-- -----------------------------------------------------------------------------
-- domain.Sport {id, slug, name}. Modelled as an entity rather than a Go enum,
-- and sport.go says why: "the set of sports is provider data — a new sport
-- appearing in a feed would then require a code change, a release, and a
-- migration to represent something the system only ever displays and groups
-- by."
--
-- That is also why this table is NOT a closed reference table and gets no seed
-- rows. It is populated by ingest from the provider's own sport keys.
--
-- The primary key is TEXT carrying domain.SportID, not a bigserial. Every
-- identifier in this system is provider-derived and must survive a rebuild of
-- the row it names; a surrogate integer would have to be kept in sync with the
-- real identity anyway, and would break the compacted-Kafka-key and
-- WebSocket-channel identity that CLAUDE.md §3 and §5 depend on. The charset
-- CHECK reproduces `domain.validID` exactly (internal/domain/ids.go): 1 to
-- MaxIDLen=128 bytes of [A-Za-z0-9._-], with ':' excluded because a colon
-- inside an identifier would make splitting an `event:{id}` channel name
-- ambiguous. It is spelled identically to 00005's users_id_charset.
CREATE TABLE sports (
    id          TEXT        PRIMARY KEY
                            CONSTRAINT sports_id_charset
                            CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.Slug. UNIQUE globally, and lowercase by CHECK rather than by
    -- convention: NewSlug rejects uppercase instead of folding it, because
    -- "folding makes 'NBA' and 'nba' two spellings of one key and someone will
    -- eventually compare the unfolded forms". The regex is NewSlug's rule
    -- exactly — first byte [a-z0-9], then up to 63 more of [a-z0-9_-], so 1 to
    -- MaxSlugLen=64 characters. Leading with an alphanumeric is what keeps a
    -- slug from ever looking like a flag or a relative path when it is
    -- interpolated into a URL.
    slug        TEXT        NOT NULL UNIQUE
                            CONSTRAINT sports_slug_charset
                            CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),

    -- domain.validateName: trimmed, non-empty, at most MaxNameLen=160 runes,
    -- no control characters. char_length() counts characters rather than bytes,
    -- which is the rune bound validateName applies; the byte length is
    -- deliberately unbounded so an accented or Cyrillic name is not penalised.
    -- UTF-8 validity needs no CHECK: the database encoding is UTF8, so an
    -- invalid sequence cannot be stored in the first place.
    name        TEXT        NOT NULL
                            CONSTRAINT sports_name_shape
                            CHECK (name = btrim(name, E' \t\n\r\f\v')
                                   AND char_length(name) BETWEEN 1 AND 160
                                   AND name !~ '[[:cntrl:]]'),

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER sports_set_updated_at
    BEFORE UPDATE ON sports
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

COMMENT ON TABLE  sports IS
    'Root of the CLAUDE.md §4 hierarchy. Provider data, not a closed reference set: populated by ingest, empty until then, and deliberately unseeded.';
COMMENT ON COLUMN sports.id IS
    'domain.SportID. Provider-derived TEXT, not a surrogate integer, so it survives a rebuild and can key a compacted Kafka topic.';
COMMENT ON COLUMN sports.slug IS
    'domain.Slug. Globally unique lowercase key that appears in URLs and operator config. Lowercase is enforced, not folded.';

-- -----------------------------------------------------------------------------
-- leagues
-- -----------------------------------------------------------------------------
-- domain.League {id, sportID, slug, name}.
--
-- `slug` is UNIQUE GLOBALLY rather than per-sport, and that is load-bearing
-- rather than tidy. CLAUDE.md §5 defines a WebSocket channel as `league:{slug}`
-- with no sport component, so two leagues sharing a slug under different sports
-- would resolve one subscription to two different sets of events — cross-
-- subscription leakage, which is the exact failure `domain.validID` excludes
-- ':' to avoid. The channel key must be a key.
--
-- ON DELETE RESTRICT on sport_id, chosen rather than defaulted. See the note on
-- events.league_id for the full argument; in short, the catalogue is the spine
-- that the price hypertable and the ledger's evidence hang off, and a cascade
-- from the root of the hierarchy would make one DELETE capable of removing
-- years of line history. Deleting a sport that still has leagues under it must
-- be refused, not silently obeyed.
CREATE TABLE leagues (
    id          TEXT        PRIMARY KEY
                            CONSTRAINT leagues_id_charset
                            CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    sport_id    TEXT        NOT NULL
                            REFERENCES sports (id) ON DELETE RESTRICT
                            CONSTRAINT leagues_sport_id_charset
                            CHECK (sport_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    slug        TEXT        NOT NULL UNIQUE
                            CONSTRAINT leagues_slug_charset
                            CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),

    name        TEXT        NOT NULL
                            CONSTRAINT leagues_name_shape
                            CHECK (name = btrim(name, E' \t\n\r\f\v')
                                   AND char_length(name) BETWEEN 1 AND 160
                                   AND name !~ '[[:cntrl:]]'),

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER leagues_set_updated_at
    BEFORE UPDATE ON leagues
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

COMMENT ON TABLE  leagues IS
    'A competition within a sport (CLAUDE.md §4). The unit users browse and subscribe to.';
COMMENT ON COLUMN leagues.slug IS
    'Globally unique, NOT unique-per-sport: CLAUDE.md §5 defines the subscription channel as league:{slug} with no sport component, so a shared slug would leak one subscription across two leagues.';
COMMENT ON COLUMN leagues.sport_id IS
    'FK to sports(id) ON DELETE RESTRICT: the catalogue is the spine the price history hangs off, so deleting a populated sport is refused rather than cascaded.';

-- Serves: the board's navigation sidebar, which lists leagues grouped by sport
-- on every page load (`SELECT ... FROM leagues WHERE sport_id = $1 ORDER BY
-- name`), and the referencing side of the sport_id FK so the RESTRICT check on
-- a sport delete is an index lookup rather than a sequential scan.
CREATE INDEX leagues_sport_idx ON leagues (sport_id);

-- -----------------------------------------------------------------------------
-- books
-- -----------------------------------------------------------------------------
-- domain.Book {id, slug, name, kind, reference}.
--
-- Not part of the Sport→…→Selection chain: a book is the other axis a Price is
-- keyed on. It has no FK in this file and nothing here references it; the price
-- hypertable does.
--
-- `kind` is not bookkeeping. book.go: "An arbitrage or +EV signal computed
-- against synthetic quotes is a statement about a random number generator", so
-- any analytics surface presenting a signal as actionable has to be able to
-- tell the two apart.
CREATE TABLE books (
    id            TEXT        PRIMARY KEY
                              CONSTRAINT books_id_charset
                              CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- UNIQUE is what makes `domain.SyntheticBookSlug` usable as a stable
    -- upsert key by the ingest startup path that creates that row (see the
    -- header). It is also the key the board's multi-book comparison columns are
    -- ordered and labelled by.
    slug          TEXT        NOT NULL UNIQUE
                              CONSTRAINT books_slug_charset
                              CHECK (slug ~ '^[a-z0-9][a-z0-9_-]{0,63}$'),

    name          TEXT        NOT NULL
                              CONSTRAINT books_name_shape
                              CHECK (name = btrim(name, E' \t\n\r\f\v')
                                     AND char_length(name) BETWEEN 1 AND 160
                                     AND name !~ '[[:cntrl:]]'),

    -- domain.BookKind.String(). BookKindUnknown is the invalid zero value and
    -- has no spelling here, deliberately: `unknown` is what String() returns
    -- for an unset value, never a state a stored book can legitimately be in.
    kind          TEXT        NOT NULL
                              CONSTRAINT books_kind_defined
                              CHECK (kind IN ('external', 'synthetic')),

    -- domain.Book.IsReference(). CLAUDE.md §6's positive-EV finder prices
    -- against "a sharp reference book"; this flag names it.
    --
    -- Orthogonal to `kind`, not derived from it — book.go is explicit that a
    -- book can be external and sharp (Pinnacle), external and soft, or
    -- synthetic and the only quote source there is in the offline path. So
    -- there is no CHECK tying the two columns together.
    is_reference  BOOLEAN     NOT NULL DEFAULT FALSE,

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TRIGGER books_set_updated_at
    BEFORE UPDATE ON books
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

-- The invariant book.go hands to storage in as many words: "That exactly one
-- book should carry this flag is a property of the catalogue, not of any single
-- Book, so it is enforced where books are stored rather than here." This is
-- where books are stored.
--
-- A partial unique index on the flag column itself: the predicate admits only
-- rows where is_reference is true, so every indexed value is `true`, and
-- uniqueness over them means at most one such row exists. Two operators
-- promoting two books concurrently is a constraint violation rather than a
-- silently ambiguous +EV baseline.
--
-- "At most one", not "exactly one" — and the gap is correct rather than a
-- weaker approximation. An empty `books` table after `make up` is the right
-- state (no seed data), and no constraint can require a row to exist in an
-- empty table. The +EV finder must therefore treat "no reference book" as a
-- legitimate empty state, which it has to do anyway before ingest first runs.
CREATE UNIQUE INDEX books_reference_unique_idx
    ON books (is_reference) WHERE is_reference;

COMMENT ON TABLE  books IS
    'A sportsbook whose lines are ingested (CLAUDE.md §4), including the in-house synthetic one. Deliberately unseeded: ingest creates the synthetic row via domain.NewSyntheticBook, which owns both the slug constant and the runtime is_reference decision.';
COMMENT ON COLUMN books.kind IS
    'domain.BookKind: external | synthetic. Load-bearing, not bookkeeping — a signal computed against synthetic quotes is a statement about an RNG and must be labelled as such.';
COMMENT ON COLUMN books.is_reference IS
    'The sharp reference book CLAUDE.md §6''s +EV finder prices against. Orthogonal to kind. At most one row may set it, enforced by books_reference_unique_idx.';

-- -----------------------------------------------------------------------------
-- events
-- -----------------------------------------------------------------------------
-- domain.Event {id, leagueID, kind, name, home, away, scheduledStart, status,
-- clock+hasClock, score+hasScore, updatedAt}.
--
-- COMPETITORS ARE DENORMALISED, WITH NO `competitors` TABLE
-- --------------------------------------------------------
-- domain.Competitor is a VALUE, not an entity: it has no lifecycle, no
-- constructor that persists it, and — critically — an OPTIONAL identifier.
-- event.go: "Providers frequently supply a display name and nothing else, and
-- refusing the event over a missing surrogate key would drop real markets."
--
-- A `competitors` table would therefore have to mint an identifier for every
-- name-only competitor, which is inventing data, and the FK from events would
-- have to be nullable-and-meaningless for exactly the rows that need it most.
-- The domain models a competitor as a value embedded in an event; the schema
-- mirrors that. If competitor-level aggregation ever earns its own entity, that
-- is a migration with a backfill and an ADR, not a column added here.
--
-- OPTIONALITY OF THE CLOCK AND THE SCORE
-- --------------------------------------
-- `Event.Clock()` and `Event.Score()` both return (value, bool), and event.go
-- says why the bool is the whole point: "an event with no clock is distinct
-- from one stopped at 0:00 in period 1."
--
-- Three candidate encodings were considered. JSONB loses every per-field CHECK
-- and the type of every field. A composite type gets whole-value NULL-ability
-- for free but cannot carry per-field constraints, and native composite types
-- are the same "second Go type for one concept" problem the TEXT-over-ENUM
-- decision rejects. Nullable columns with an all-or-nothing CHECK keep the
-- per-field bounds AND make absence a single unambiguous state, so that is what
-- is used: `num_nulls(...) IN (0, 3)` admits "all set" and "none set" and
-- rejects every partial combination.
CREATE TABLE events (
    id                    TEXT        PRIMARY KEY
                                      CONSTRAINT events_id_charset
                                      CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- ON DELETE RESTRICT, chosen rather than defaulted, and this is the FK the
    -- decision really turns on.
    --
    -- CASCADE reads as the natural choice — an event is meaningless without its
    -- league — but follow what it would reach. events → markets → selections,
    -- and the price hypertable keys on selections and books. CLAUDE.md §3 calls
    -- line history "the interesting dataset"; §4 says a price is immutable and
    -- a new price is a new row. Under CASCADE, one DELETE against a stale
    -- league would silently destroy every price ever observed under it.
    --
    -- Nothing in the domain deletes anything: there is no Delete method on any
    -- entity and no status transition that removes a row (a cancelled event
    -- moves to `settled`, it does not disappear). So RESTRICT costs the system
    -- nothing it does today, and buys the property that retracting real data is
    -- an explicit, ordered, four-statement operation a human had to mean.
    league_id             TEXT        NOT NULL
                                      REFERENCES leagues (id) ON DELETE RESTRICT
                                      CONSTRAINT events_league_id_charset
                                      CHECK (league_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.EventKind.String().
    kind                  TEXT        NOT NULL
                                      CONSTRAINT events_kind_defined
                                      CHECK (kind IN ('match', 'outright')),

    -- Display name, stored rather than derived. event.go: "For a match it is
    -- typically 'Away at Home'; for an outright it is the competition ('2027
    -- NBA Champion'). It is stored rather than derived so that the provider's
    -- own wording survives."
    name                  TEXT        NOT NULL
                                      CONSTRAINT events_name_shape
                                      CHECK (name = btrim(name, E' \t\n\r\f\v')
                                             AND char_length(name) BETWEEN 1 AND 160
                                             AND name !~ '[[:cntrl:]]'),

    -- domain.Competitor.ID() — OPTIONAL, per event.go. NULL means the provider
    -- supplied a name and no identifier, which is routine, not degraded.
    home_competitor_id    TEXT
                                      CONSTRAINT events_home_competitor_id_charset
                                      CHECK (home_competitor_id IS NULL
                                             OR home_competitor_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.Competitor.Name(). Required whenever a competitor is present, and
    -- therefore the column the presence of a competitor is read from: a
    -- Competitor with an id and no name is unconstructible, because
    -- NewCompetitor validates the name unconditionally.
    home_competitor_name  TEXT
                                      CONSTRAINT events_home_competitor_name_shape
                                      CHECK (home_competitor_name IS NULL
                                             OR (home_competitor_name = btrim(home_competitor_name, E' \t\n\r\f\v')
                                                 AND char_length(home_competitor_name) BETWEEN 1 AND 160
                                                 AND home_competitor_name !~ '[[:cntrl:]]')),

    away_competitor_id    TEXT
                                      CONSTRAINT events_away_competitor_id_charset
                                      CHECK (away_competitor_id IS NULL
                                             OR away_competitor_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    away_competitor_name  TEXT
                                      CONSTRAINT events_away_competitor_name_shape
                                      CHECK (away_competitor_name IS NULL
                                             OR (away_competitor_name = btrim(away_competitor_name, E' \t\n\r\f\v')
                                                 AND char_length(away_competitor_name) BETWEEN 1 AND 160
                                                 AND away_competitor_name !~ '[[:cntrl:]]')),

    -- The advertised start instant, UTC by domain construction.
    --
    -- The lower bound is a unit/parse guard in the spirit of event.go's
    -- MaxClockPeriod comment — it exists "to catch a unit error in a provider
    -- adapter", specifically Go's zero time.Time (0001-01-01T00:00:00Z) leaking
    -- through a path that forgot to check IsZero, or a Unix epoch of 0. It is
    -- set far enough back that no real fixture, historical or future, can trip
    -- it. NewEvent already rejects the zero time; this is the same rule stated
    -- where a hand-written INSERT can also be caught by it.
    scheduled_start       TIMESTAMPTZ NOT NULL
                                      CONSTRAINT events_scheduled_start_sane
                                      CHECK (scheduled_start > '1900-01-01T00:00:00Z'),

    -- domain.EventStatus.String(). All seven defined statuses; EventStatusUnknown
    -- has no spelling here.
    status                TEXT        NOT NULL
                                      CONSTRAINT events_status_defined
                                      CHECK (status IN ('scheduled', 'live', 'suspended',
                                                        'ended', 'settled', 'postponed',
                                                        'cancelled')),

    -- domain.GameClock.Period(). 1-based; the bound is domain.MaxClockPeriod.
    clock_period          INTEGER
                                      CONSTRAINT events_clock_period_range
                                      CHECK (clock_period IS NULL
                                             OR clock_period BETWEEN 1 AND 50),

    -- domain.GameClock.Elapsed(), in NANOSECONDS — the unit of time.Duration
    -- itself, so the mapping is `int64(d)` and the round trip is exact.
    --
    -- INTERVAL is the natural PostgreSQL type and was rejected: pgx represents
    -- it at microsecond resolution, so a Duration would not survive a round
    -- trip, and phase 1 spent real effort making round trips total. BIGINT
    -- milliseconds was rejected for the same reason at a coarser scale. A game
    -- clock does not NEED nanoseconds; a lossless store does, and the column
    -- name says which unit it is so nobody has to guess.
    --
    -- The bound is domain.MaxClockElapsed = 6h, expressed in nanoseconds. Both
    -- ends matter: NewGameClock rejects a negative elapsed as a parse error.
    --
    -- Elapsed, not remaining, per event.go: soccer counts up, basketball counts
    -- down, baseball has no clock, and only "elapsed" is universal across all
    -- three.
    clock_elapsed_ns      BIGINT
                                      CONSTRAINT events_clock_elapsed_range
                                      CHECK (clock_elapsed_ns IS NULL
                                             OR clock_elapsed_ns BETWEEN 0 AND 21600000000000),

    -- domain.GameClock.Running().
    clock_running         BOOLEAN,

    -- domain.Score.Home() / .Away(). Non-negative: NewScore rejects negatives
    -- because "every scoring system Sharpline covers is non-negative, so a
    -- negative value is a parse error rather than a legitimate reading".
    score_home            INTEGER
                                      CONSTRAINT events_score_home_nonnegative
                                      CHECK (score_home IS NULL OR score_home >= 0),
    score_away            INTEGER
                                      CONSTRAINT events_score_away_nonnegative
                                      CHECK (score_away IS NULL OR score_away >= 0),

    -- domain.Event.UpdatedAt() — the PROVIDER'S observation instant and the
    -- monotonicity guard for out-of-order bus delivery. See the header for why
    -- this is not called updated_at and for the ON CONFLICT ... WHERE clause
    -- phase 3 owes it.
    observed_at           TIMESTAMPTZ NOT NULL
                                      CONSTRAINT events_observed_at_sane
                                      CHECK (observed_at > '1900-01-01T00:00:00Z'),

    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- NewEvent's kind/competitor rule, made unstorable-if-violated. event.go:
    -- match requires both sides, outright requires neither, and collapsing the
    -- two shapes would mean "either giving every futures event two fake
    -- competitors or making Home and Away optional on every event".
    --
    -- This is a genuine ROW invariant, not merely a write-time rule: no With*
    -- method on Event changes kind or either competitor, so what NewEvent
    -- enforces at construction holds for the life of the value. `ELSE FALSE`
    -- rather than an open CASE, because a CASE with no match yields NULL and a
    -- CHECK evaluating to NULL passes.
    CONSTRAINT events_competitors_match_kind
        CHECK (CASE kind
                   WHEN 'match' THEN
                       home_competitor_name IS NOT NULL
                       AND away_competitor_name IS NOT NULL
                   WHEN 'outright' THEN
                       home_competitor_name IS NULL
                       AND away_competitor_name IS NULL
                       AND home_competitor_id IS NULL
                       AND away_competitor_id IS NULL
                   ELSE FALSE
               END),

    -- NewCompetitor validates the name unconditionally and the id only when
    -- present, so an id without a name is unconstructible in Go. Making it
    -- unstorable in SQL too means a hand-written INSERT cannot produce a
    -- competitor the domain could never have built.
    CONSTRAINT events_competitor_id_needs_name
        CHECK ((home_competitor_id IS NULL OR home_competitor_name IS NOT NULL)
               AND (away_competitor_id IS NULL OR away_competitor_name IS NOT NULL)),

    -- The (GameClock, bool) pair, faithfully. All three columns set, or all
    -- three NULL. Nothing in between, so "no clock" can never be mistaken for
    -- "period 1, 0:00, stopped" — which is itself a legal clock and a different
    -- fact.
    CONSTRAINT events_clock_all_or_nothing
        CHECK (num_nulls(clock_period, clock_elapsed_ns, clock_running) IN (0, 3)),

    -- Event.WithClock rejects a clock in any status that is not in play, and
    -- Event.WithStatus CLEARS the clock on leaving in play ("an ended event
    -- that still reported 'Q3 7:34' would be a lie that the UI would happily
    -- render"). Because the clearing half exists, this is a row invariant and
    -- not just a write-time rule, so it belongs here.
    CONSTRAINT events_clock_only_in_play
        CHECK (clock_period IS NULL OR status IN ('live', 'suspended')),

    -- The (Score, bool) pair. Both columns or neither.
    --
    -- Note what is deliberately NOT here: a constraint that a score implies a
    -- started status. Event.WithScore does require Status.HasStarted(), but
    -- Event.WithStatus does NOT clear the score, and `suspended → postponed` is
    -- a legal edge (event.go: "a match abandoned after a weather delay is
    -- routinely rescheduled"). So an event scored while live and then postponed
    -- is a legal domain value whose status does not satisfy HasStarted(), and a
    -- row invariant asserting otherwise would make it unstorable. A write-time
    -- rule and a row invariant are different claims; only the second belongs in
    -- a CHECK.
    CONSTRAINT events_score_all_or_nothing
        CHECK (num_nulls(score_home, score_away) IN (0, 2))
);

CREATE TRIGGER events_set_updated_at
    BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

COMMENT ON TABLE  events IS
    'A contest markets are offered on (CLAUDE.md §4). Competitors are denormalised values, not a competitors table, because domain.Competitor has an optional identifier and no lifecycle.';
COMMENT ON COLUMN events.observed_at IS
    'domain.Event.UpdatedAt(): the PROVIDER observation instant and the monotonicity guard for out-of-order Kafka delivery. NOT a row-modification time — that is updated_at. Writers must guard it with ON CONFLICT ... WHERE excluded.observed_at >= events.observed_at.';
COMMENT ON COLUMN events.clock_elapsed_ns IS
    'domain.GameClock.Elapsed() in nanoseconds, the unit of time.Duration, so the round trip is exact. INTERVAL was rejected: pgx renders it at microsecond resolution.';
COMMENT ON COLUMN events.clock_period IS
    'domain.GameClock.Period(), 1-based (quarter, half, inning, round). NULL with the other two clock columns means no clock at all, which is a different fact from a stopped clock at 0:00.';
COMMENT ON COLUMN events.home_competitor_id IS
    'Optional: providers frequently supply a name and no identifier, and refusing the event over a missing surrogate key would drop real markets.';

-- -----------------------------------------------------------------------------
-- markets
-- -----------------------------------------------------------------------------
-- domain.Market {id, eventID, typ, line, subject, status, updatedAt}.
--
-- THE LINE
-- --------
-- market.go's Line type exists for one sentence in CLAUDE.md §4 — a market
-- "carries a market type and a line where applicable" — and the whole design is
-- the phrase "where applicable": "A moneyline has no line; a pick'em spread has
-- a line and that line is exactly 0.0, which is a real, meaningful,
-- frequently-traded value. A bare float64 cannot tell those two apart, and the
-- failure mode is silent."
--
-- So the column is NULLABLE and absence is NULL. 0.0 is a stored pick'em, NULL
-- is no line, and the two are distinguishable by construction. The alternative
-- the schema conventions allow — a NOT NULL value plus a NOT NULL present flag
-- — was rejected because it admits the one state that means nothing
-- (present = false with a value of 4.5) and SQL has a purpose-built encoding
-- for optionality already.
CREATE TABLE markets (
    id            TEXT        PRIMARY KEY
                              CONSTRAINT markets_id_charset
                              CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- ON DELETE RESTRICT for the same reason as events.league_id: this is the
    -- row the price hypertable's selections hang off, and a cascade from an
    -- event would reach line history. Note that 00007's market_suspensions
    -- correctly uses CASCADE in the other direction — a suspension episode is a
    -- leaf property OF a market with nothing below it, so cascading there
    -- destroys nothing. Direction and depth are what make one right and the
    -- other wrong; neither is a default.
    event_id      TEXT        NOT NULL
                              REFERENCES events (id) ON DELETE RESTRICT
                              CONSTRAINT markets_event_id_charset
                              CHECK (event_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- domain.MarketType.String(). Exactly the five CLAUDE.md §4 names.
    -- market.go: "Team totals, alternate lines, and period markets are all real
    -- and all tempting, but adding a type here without a corresponding pricing
    -- and grading rule elsewhere produces a market the system can quote and
    -- cannot settle." Widening this CHECK without the matching Go constant and
    -- grading rule is that mistake.
    type          TEXT        NOT NULL
                              CONSTRAINT markets_type_defined
                              CHECK (type IN ('moneyline', 'spread', 'total',
                                              'player_prop', 'futures')),

    -- domain.Line. NULL is Line{} / NoLine(); a value is a present Line.
    --
    -- For a spread this is stated FROM THE HOME SIDE'S PERSPECTIVE, per the
    -- convention documented on domain.Market: -3.5 means the home competitor
    -- gives 3.5 points, and the away side of the same market is +3.5 via
    -- Line.Invert(). The line is stored ONCE, on the market, precisely because
    -- "storing a line per selection duplicates one number across two rows that
    -- can then disagree". Readers must go through domain.EffectiveLine rather
    -- than reading this column and remembering to invert.
    --
    -- DOUBLE PRECISION per CLAUDE.md §12 (lines and odds are floats; only money
    -- is not). The finiteness CHECK reproduces NewLine, which rejects NaN and
    -- ±Inf because they are "what a division-by-zero or a failed parse produces
    -- upstream" and they propagate silently through every later comparison.
    --
    -- The comparison form is deliberate: PostgreSQL defines NaN as EQUAL to
    -- itself and GREATER than every other float8 (unlike IEEE), so `line = line`
    -- would not catch it. `line < 'Infinity'` does — NaN sorts above Infinity,
    -- so the test fails and the row is refused.
    line          DOUBLE PRECISION
                              CONSTRAINT markets_line_finite
                              CHECK (line IS NULL
                                     OR (line > '-Infinity'::double precision
                                         AND line < 'Infinity'::double precision)),

    -- domain.Market.Subject(): the individual a player prop is about ("LeBron
    -- James"). NULL rather than '' for absent, so that the NeedsSubject rule
    -- below is a null-test rather than a string comparison. The domain stores ""
    -- for absent because Go has no nullable string; SQL does, and NULL is the
    -- encoding that makes the constraint expressible.
    subject       TEXT
                              CONSTRAINT markets_subject_shape
                              CHECK (subject IS NULL
                                     OR (subject = btrim(subject, E' \t\n\r\f\v')
                                         AND char_length(subject) BETWEEN 1 AND 160
                                         AND subject !~ '[[:cntrl:]]')),

    -- domain.MarketStatus.String(). Independent of the event's status, because
    -- CLAUDE.md §6 gives an admin the power to suspend one market while the rest
    -- of the event trades on — which is also why there is no CHECK correlating
    -- this column with events.status.
    status        TEXT        NOT NULL
                              CONSTRAINT markets_status_defined
                              CHECK (status IN ('open', 'suspended', 'closed',
                                                'settled', 'voided')),

    -- domain.Market.UpdatedAt(): the provider observation instant. Same
    -- contract as events.observed_at — see the header.
    observed_at   TIMESTAMPTZ NOT NULL
                              CONSTRAINT markets_observed_at_sane
                              CHECK (observed_at > '1900-01-01T00:00:00Z'),

    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- domain.MarketType.LineRule(), enforced so that an illegal market cannot
    -- be STORED rather than merely not constructed. This is the constraint the
    -- table exists to get right: a spread with no line is unpriceable and a
    -- moneyline with one is a display bug, and both would otherwise sit in the
    -- database looking plausible.
    --
    --   moneyline, futures  → LineRuleForbidden
    --   spread, total       → LineRuleRequired   (spread may be 0.0: pick'em)
    --   player_prop         → LineRuleOptional   ("over 24.5 points" has one,
    --                                             "first to score" does not)
    --
    -- The `total > 0` half is MarketType.validateLine's extra rule: "a total is
    -- a threshold on combined scoring, which is non-negative by construction,
    -- so a zero or negative total is a parse error rather than a tradeable
    -- market. A spread has no such restriction: zero is a pick'em and negative
    -- is the favourite's handicap."
    CONSTRAINT markets_line_rule
        CHECK (CASE type
                   WHEN 'moneyline'   THEN line IS NULL
                   WHEN 'futures'     THEN line IS NULL
                   WHEN 'spread'      THEN line IS NOT NULL
                   WHEN 'total'       THEN line IS NOT NULL AND line > 0
                   WHEN 'player_prop' THEN TRUE
                   ELSE FALSE
               END),

    -- domain.MarketType.NeedsSubject(), which is true for player props and
    -- nothing else. NewMarket rejects a missing subject on a prop AND a present
    -- subject on any other type, so the biconditional is exact rather than a
    -- one-sided approximation.
    CONSTRAINT markets_subject_matches_type
        CHECK ((type = 'player_prop') = (subject IS NOT NULL)),

    -- Target for the composite foreign key on `selections`. See that table for
    -- why the denormalised copy exists; this UNIQUE is what makes it
    -- unforgeable. It is trivially satisfied — `id` is already the primary key,
    -- so the pair cannot repeat — and costs one narrow secondary index, which
    -- is the entire price of enforcing MarketType.AllowsRole() in the database
    -- instead of hoping every writer remembers it.
    CONSTRAINT markets_id_type_key UNIQUE (id, type)
);

CREATE TRIGGER markets_set_updated_at
    BEFORE UPDATE ON markets
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

COMMENT ON TABLE  markets IS
    'A question about an event (CLAUDE.md §4). The line/subject legality rules from domain.MarketType.LineRule() and NeedsSubject() are enforced as CHECKs, so an illegal market cannot be stored.';
COMMENT ON COLUMN markets.line IS
    'domain.Line. NULL means NO line; 0.0 means a pick''em, which is a real traded value. For a spread the value is from the HOME side''s perspective — read it through domain.EffectiveLine, never directly.';
COMMENT ON COLUMN markets.subject IS
    'The individual a player prop is about. NULL for every other market type, enforced as a biconditional against `type`.';
COMMENT ON COLUMN markets.status IS
    'domain.MarketStatus. Deliberately independent of events.status: CLAUDE.md §6 lets an admin suspend one market while the event trades on.';

-- -----------------------------------------------------------------------------
-- selections
-- -----------------------------------------------------------------------------
-- domain.Selection {id, marketID, role, name}.
--
-- WHY `market_type` IS DUPLICATED HERE
-- -----------------------------------
-- `domain.MarketType.AllowsRole()` is a compatibility matrix between two enums
-- that live on two different tables:
--
--     moneyline   → home, away, draw
--     spread      → home, away
--     total       → over, under
--     player prop → over, under, outright
--     futures     → outright
--
-- A CHECK constraint cannot read another table, so enforcing this needs either
-- a trigger or a denormalised copy of the parent's type. The copy wins:
--
--   * It cannot drift. The foreign key is on the PAIR
--     `(market_id, market_type) → markets (id, type)`, so a row whose
--     market_type disagrees with its parent's type is not merely wrong, it is
--     unstorable. `ON UPDATE CASCADE` means a (hypothetical) type correction
--     propagates rather than blocking — markets.type never changes today, since
--     NewMarket sets it and no With* method touches it.
--   * It is declarative, so it holds under COPY and bulk load, where a
--     row-level trigger is both slower and easier to disable.
--   * The pair FK subsumes a plain `market_id → markets(id)` reference: the
--     pair must exist in `markets`, which implies the id does.
--
-- The cost is one redundant TEXT column and one UNIQUE index on the parent.
-- What it buys is that a 'draw' selection on a spread market — which is
-- unpriceable, ungradeable, and would silently break the devig fold — cannot
-- reach the database. market.go's own comment on why spreads have no draw ("the
-- handicap is quoted in half points precisely to eliminate the tie") is the kind
-- of rule that is obvious until someone writes a normalizer at 2am.
CREATE TABLE selections (
    id           TEXT        PRIMARY KEY
                             CONSTRAINT selections_id_charset
                             CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    market_id    TEXT        NOT NULL
                             CONSTRAINT selections_market_id_charset
                             CHECK (market_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Denormalised copy of markets.type, held honest by the composite FK below.
    -- Never written independently: it is whatever the parent market's type is,
    -- and the database will refuse any other value.
    market_type  TEXT        NOT NULL,

    -- domain.SelectionRole.String().
    role         TEXT        NOT NULL,

    -- domain.Selection.Name(): "the provider's wording, kept rather than
    -- derived, because for an outright it is the only thing that identifies the
    -- runner."
    name         TEXT        NOT NULL
                             CONSTRAINT selections_name_shape
                             CHECK (name = btrim(name, E' \t\n\r\f\v')
                                    AND char_length(name) BETWEEN 1 AND 160
                                    AND name !~ '[[:cntrl:]]'),

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- ON DELETE RESTRICT for the same reason as the FKs above: the price
    -- hypertable keys on selections(id), so cascading from a market would reach
    -- line history.
    CONSTRAINT selections_market_fk
        FOREIGN KEY (market_id, market_type)
        REFERENCES markets (id, type)
        ON DELETE RESTRICT ON UPDATE CASCADE,

    -- domain.MarketType.AllowsRole(), verbatim.
    --
    -- This single constraint also subsumes the two value-set checks that would
    -- otherwise be written separately: the union of the role lists below is
    -- exactly SelectionRole's six defined values, and the CASE arms are exactly
    -- MarketType's five, with `ELSE FALSE` refusing anything else. Adding
    -- redundant `IN` checks alongside it would cost write time and create a
    -- second place for the value sets to be edited inconsistently.
    CONSTRAINT selections_role_allowed
        CHECK (CASE market_type
                   WHEN 'moneyline'   THEN role IN ('home', 'away', 'draw')
                   WHEN 'spread'      THEN role IN ('home', 'away')
                   WHEN 'total'       THEN role IN ('over', 'under')
                   WHEN 'player_prop' THEN role IN ('over', 'under', 'outright')
                   WHEN 'futures'     THEN role = 'outright'
                   ELSE FALSE
               END)
);

CREATE TRIGGER selections_set_updated_at
    BEFORE UPDATE ON selections
    FOR EACH ROW EXECUTE FUNCTION catalogue_set_updated_at();

COMMENT ON TABLE  selections IS
    'One answer to a market''s question (CLAUDE.md §4). Carries a denormalised market_type, pinned to the parent by a composite FK, so that domain.MarketType.AllowsRole() is enforced declaratively.';
COMMENT ON COLUMN selections.market_type IS
    'Denormalised copy of markets.type. Cannot drift: the FK is on (market_id, market_type) → markets(id, type). Exists solely so AllowsRole() can be a CHECK rather than a trigger.';
COMMENT ON COLUMN selections.role IS
    'domain.SelectionRole. Legality against the market type is enforced by selections_role_allowed. Render order is domain.SelectionRole.DisplayOrder(), which is not lexicographic and is therefore not indexable.';

-- A SCHEMA INVARIANT WITH NO DOMAIN COUNTERPART, AND WHY IT IS HERE ANYWAY.
--
-- A market may hold at most one selection per role, except for 'outright' where
-- a field of many runners is the whole point (a futures market has forty, a
-- player prop can name several outcomes).
--
-- domain.Selection cannot express this: it holds only its parent's identifier
-- and knows nothing about its siblings, so no constructor is in a position to
-- check it. But it is structurally true of every market type — one home, one
-- away, one draw, one over, one under — and phase 4 depends on it. Devigging
-- folds over a market's selections and asserts the no-vig probabilities sum to
-- one (CLAUDE.md §4); a duplicated 'over' makes ImpliedSum silently wrong with
-- no error anywhere, which is precisely the bug class CLAUDE.md §10 says
-- destroys the project's credibility. A duplicate is a normalizer bug, and this
-- index turns it into a constraint violation at the moment it is written
-- instead of a wrong number on a dashboard a week later.
CREATE UNIQUE INDEX selections_one_per_role_idx
    ON selections (market_id, role) WHERE role <> 'outright';

-- =============================================================================
-- INDEXES
--
-- Derived from the reads that will actually exist, not from the column list.
-- Every index below names the query it serves. CLAUDE.md §5 puts ingest on the
-- hot write path with change detection suppressing no-op updates, so an index
-- nobody queries is pure write amplification there — which is why, for example,
-- there is no index on markets.status or events.kind.
--
-- Primary keys and UNIQUE constraints already declared above are indexes too,
-- and they are what serve several of the required patterns. Those are recorded
-- here rather than duplicated as new indexes.
-- =============================================================================

-- Serves: the odds board's per-league view — "live and upcoming events for a
-- league, ordered by start time":
--
--     SELECT ... FROM events
--      WHERE league_id = $1
--        AND status IN ('scheduled', 'live', 'suspended')
--      ORDER BY scheduled_start;
--
-- (league_id, scheduled_start) makes that an index scan with no sort. The
-- partial predicate keeps settled, cancelled and postponed events out: they
-- accumulate without bound as the season runs and are never on the board, so
-- excluding them keeps the index proportional to what is tradeable rather than
-- to all history. The predicate is IMMUTABLE (text equality against literals),
-- which a partial index requires.
CREATE INDEX events_league_board_idx
    ON events (league_id, scheduled_start)
    WHERE status IN ('scheduled', 'live', 'suspended');

-- Serves TWO real queries with one index, which is why it is not folded into
-- the one above:
--
--   1. the cross-league board — CLAUDE.md §6 Core: "live odds board across
--      leagues" — ordered by start time with no league filter, which cannot use
--      an index whose leading column is league_id;
--
--   2. ingest's adaptive-polling scheduler (CLAUDE.md §5: "high frequency for
--      live and near-tip events, low for futures"), which asks for the events
--      inside a start-time window on every scheduling tick:
--
--          SELECT id FROM events
--           WHERE status IN ('scheduled', 'live', 'suspended')
--             AND scheduled_start < now() + interval '1 hour'
--           ORDER BY scheduled_start;
--
-- The second is the higher-frequency read in the system by a wide margin, and
-- it runs on the ingest path itself.
CREATE INDEX events_open_start_idx
    ON events (scheduled_start)
    WHERE status IN ('scheduled', 'live', 'suspended');

-- Serves: the event detail page loading a whole market tree by event_id
-- (CLAUDE.md §6 Core: "event detail with full market tree"), and the board's
-- per-event main markets:
--
--     SELECT ... FROM markets WHERE event_id = $1;
--     SELECT ... FROM markets
--      WHERE event_id = ANY($1)
--        AND type IN ('moneyline', 'spread', 'total');
--
-- `type` is the second column rather than omitted because the board's row for
-- an event asks for three of the five types and nothing else, and it costs one
-- short text per entry. It also serves the referencing side of the event_id FK,
-- so the RESTRICT check on an event delete is a lookup rather than a scan.
CREATE INDEX markets_event_type_idx ON markets (event_id, type);

-- Serves: the second level of the same market tree, and the referencing side of
-- the composite FK to markets:
--
--     SELECT ... FROM selections WHERE market_id = ANY($1);
--
-- `role` is deliberately NOT a trailing column. The render order is
-- domain.SelectionRole.DisplayOrder() — home, draw, away, over, under, outright
-- — which is not the lexicographic order of those strings, so an index on role
-- could not produce the display order anyway. The sort happens in the
-- application, over the handful of rows one market has.
CREATE INDEX selections_market_idx ON selections (market_id);

-- Serves: search and filtering by competitor name (CLAUDE.md §6 Core: "search
-- and filtering"):
--
--     SELECT ... FROM events
--      WHERE lower(home_competitor_name) LIKE lower($1) || '%';
--
-- `text_pattern_ops` is what makes a prefix LIKE indexable independently of the
-- database collation; `lower(...)` on both sides makes the match
-- case-insensitive without a second stored column. Both indexes are partial on
-- IS NOT NULL, so outright events — which have no competitors at all — occupy
-- no space in either.
--
-- Two indexes rather than one because a competitor can appear on either side of
-- a fixture and a user searching "celtics" means either. There is no single
-- expression over both columns that a prefix search can use.
--
-- LIMITATION, STATED PLAINLY: this serves PREFIX search only. Infix search —
-- typing "lakers" and expecting "Los Angeles Lakers" — wants a GIN trigram
-- index, which requires the `pg_trgm` extension. This migration deliberately
-- does not create that extension: it is shared infrastructure, `CREATE
-- EXTENSION IF NOT EXISTS` would make this file's Down capable of dropping
-- something the bootstrap migration owns, and a conditionally-created index
-- would make the schema differ between laptop and cluster — the exact failure
-- mode CLAUDE.md §9 cites as Terraform's justification. The trigram index is a
-- follow-up migration once `pg_trgm` exists; until then the API's search
-- endpoint should be prefix-matched, which is also what a type-ahead box wants.
CREATE INDEX events_home_competitor_search_idx
    ON events (lower(home_competitor_name) text_pattern_ops)
    WHERE home_competitor_name IS NOT NULL;

CREATE INDEX events_away_competitor_search_idx
    ON events (lower(away_competitor_name) text_pattern_ops)
    WHERE away_competitor_name IS NOT NULL;

-- Phase 6's WebSocket subscription keys (CLAUDE.md §5: `event:{id}`,
-- `market:{id}`, `league:{slug}`) are ALREADY fully served, and no index is
-- added for them. Recorded explicitly so the omission reads as a decision:
--
--   event:{id}     → events.id, the primary key.
--   market:{id}    → markets.id, the primary key.
--   league:{slug}  → leagues.slug UNIQUE resolves slug → league_id, then
--                    events_league_board_idx resolves league_id → its events.
--
-- Routing in the other direction — a price delta arrives keyed by market_id and
-- the hub must decide which channels it belongs to — walks markets.id →
-- events.id → leagues.id → slug, three primary-key lookups. CLAUDE.md §9
-- requires `stream` to be horizontally scalable with no session affinity, which
-- means that mapping is cached in Redis rather than re-queried per delta; the
-- primary keys are what the cache is populated from.

-- +goose Down

-- Reversibility, per CLAUDE.md §12: "Migrations are forward-only, and every one
-- is reversible in review before it is applied." This Down is the exact inverse
-- of the Up and is proven to run. It is the review artifact, not an operational
-- tool: running it against a database holding real catalogue data destroys that
-- data, and the RESTRICT foreign keys above cannot stop it, because DROP TABLE
-- is not a row operation.
--
-- Order matters in three ways. Indexes before their tables is cosmetic (DROP
-- TABLE takes its own indexes with it) but explicit, matching 00005. Tables in
-- reverse dependency order is not cosmetic: selections references markets
-- references events references leagues references sports, and dropping a parent
-- before its child fails on the dependency. The trigger function comes last,
-- after every trigger that calls it has gone with its table.
--
-- Nothing outside this file is touched: no extension is dropped, and the only
-- function removed is the `catalogue_`-prefixed one this migration created.

DROP INDEX IF EXISTS events_away_competitor_search_idx;
DROP INDEX IF EXISTS events_home_competitor_search_idx;
DROP INDEX IF EXISTS selections_market_idx;
DROP INDEX IF EXISTS markets_event_type_idx;
DROP INDEX IF EXISTS events_open_start_idx;
DROP INDEX IF EXISTS events_league_board_idx;
DROP INDEX IF EXISTS selections_one_per_role_idx;
DROP INDEX IF EXISTS books_reference_unique_idx;
DROP INDEX IF EXISTS leagues_sport_idx;

DROP TABLE IF EXISTS selections;
DROP TABLE IF EXISTS markets;
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS books;
DROP TABLE IF EXISTS leagues;
DROP TABLE IF EXISTS sports;

DROP FUNCTION IF EXISTS catalogue_set_updated_at();
