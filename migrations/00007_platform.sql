-- Platform / operational schema.
--
-- CLAUDE.md §6 "Platform": "admin console for market suspension and manual
-- settlement; feature flags; audit log on every state-changing action; rate
-- limiting per user and per IP."
--
-- Three tables live here:
--
--   audit_log           append-only, hypertable, the forensic record of every
--                       state-changing action in the system
--   feature_flags       operator-controlled runtime switches
--   market_suspensions  the admin console's market-suspension workflow: who
--                       suspended what, why, and whether it is still suspended
--
-- ---------------------------------------------------------------------------
-- WHAT IS DELIBERATELY *NOT* HERE
-- ---------------------------------------------------------------------------
--
-- No rate-limit counters. No idempotency keys. No session store. No nonce
-- table. CLAUDE.md §3 assigns all of those to Redis — "current-line snapshot
-- cache, WebSocket presence, distributed rate limiting, idempotency keys" —
-- and in the same breath says Redis is "Never the source of truth."
--
-- Both halves of that sentence are load-bearing. Rate-limit and idempotency
-- state is high-churn, per-request, TTL-scoped, and *disposable*: losing it
-- costs one duplicated request or one extra allowed call, never a wrong
-- balance. Putting it in Postgres would drag the one durable, fsync-ing,
-- double-entry-ledger-bearing store into the hottest write path in the system
-- for data whose correct behaviour on loss is "shrug". `synchronous_commit` is
-- deliberately ON for this database (deploy/postgres/postgresql.conf); paying
-- an fsync per rate-limit tick is exactly the trade that setting exists to
-- protect.
--
-- If you are here to add a `rate_limits` or `idempotency_keys` table: don't.
-- The charter already answered this. Change the charter first, with an ADR.
--
-- No seed data. Not one row. CLAUDE.md's data flow is
-- `provider → ingest → Kafka → normalizer → pricer → Postgres` and every value
-- a user sees must have travelled it. An empty `feature_flags` after `make up`
-- is CORRECT — flags are created by an operator through the admin console.
-- Note that none of these three tables is a lookup/reference table either:
-- their closed value sets (`actor_kind`, `outcome`) are expressed as CHECK
-- constraints, not as rows, so there is nothing to seed by construction.
--
-- No money columns. Nothing on this page is denominated in currency, so
-- CLAUDE.md §12's "all money and stake values are integer minor units" has no
-- BIGINT to claim here. If a future column on these tables ever does hold
-- money, it is BIGINT minor units — never NUMERIC, never DOUBLE PRECISION.
--
-- ---------------------------------------------------------------------------
-- ENUM REPRESENTATION: TEXT + CHECK, NOT NATIVE ENUM TYPES
-- ---------------------------------------------------------------------------
--
-- Stated once, here, because the convention must be identical across every
-- migration in this directory:
--
--   1. Reversibility. CLAUDE.md §12 requires every migration to be reversible
--      in review. Retiring a value from a native ENUM is not a DDL statement —
--      it requires creating a replacement type, rewriting every column that
--      uses it, and dropping the old one. Dropping a CHECK constraint is one
--      line, and its inverse is one line.
--   2. Transaction safety. goose wraps each migration in a transaction.
--      `ALTER TYPE ... ADD VALUE` has famously sharp edges inside one: the new
--      label is not usable by other statements in the same transaction. A
--      CHECK constraint has no such rule.
--   3. One source of truth. `internal/domain` already owns these value sets
--      and already serializes them as lowercase snake_case text — see
--      `AccountKind.String()` → "user_cash", `MarketStatus.String()` →
--      "suspended". A native ENUM makes sqlc emit a *second* Go type for the
--      same closed set, and two types for one concept drift. TEXT maps to
--      `string`, which the domain's existing `Parse*` constructors validate.
--
-- Which value sets are closed here, and which are deliberately open:
--
--   CLOSED (CHECK with a literal value list) — `actor_kind` and `outcome`.
--     These describe the audit *record itself*, they are structural to this
--     table's meaning, and each has exactly one counterpart constant required
--     of `internal/audit` (see the cross-agent note at the bottom of this
--     comment block).
--
--   OPEN (CHECK on shape only: charset, length, non-emptiness) — `action`,
--     `entity_type`, and `market_suspensions.reason`. These are vocabularies
--     spanning the whole domain, and they grow every time a new auditable
--     operation is written. Freezing them in SQL ahead of the Go constants
--     that name them would invert the frozen convention "every enum value must
--     match a constant in internal/domain" — it would mint SQL values with no
--     domain constant behind them. Shape is constrained now; the value set is
--     closed later, in its own migration, once `internal/domain` names it.
--
-- ---------------------------------------------------------------------------
-- HARD DEPENDENCIES ON MIGRATIONS OWNED BY OTHER AGENTS
-- ---------------------------------------------------------------------------
--
--   * the `timescaledb` extension must already be created (bootstrap migration)
--   * `markets (id TEXT PRIMARY KEY)` must already exist (catalogue migration)
--
-- Both are asserted by an explicit preflight below so that a missing
-- prerequisite fails with a sentence a human can act on, rather than with
-- `function create_hypertable(...) does not exist`.

-- +goose Up

-- ---------------------------------------------------------------------------
-- Preflight. Fails loudly and diagnosably, per CLAUDE.md §12 "fail fast and
-- loudly on a bad config" applied to schema prerequisites.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $preflight$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        RAISE EXCEPTION
            'migration 00007_platform requires the timescaledb extension, which is not installed'
            USING HINT = 'The bootstrap migration must run CREATE EXTENSION timescaledb before this one. '
                         'audit_log is a hypertable (see the justification in this file).';
    END IF;

    IF to_regclass('public.markets') IS NULL THEN
        RAISE EXCEPTION
            'migration 00007_platform requires the markets table, which does not exist'
            USING HINT = 'market_suspensions carries a foreign key to markets(id). '
                         'The catalogue migration must be numbered lower than 00007.';
    END IF;
END
$preflight$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- Shared trigger function: maintain updated_at on mutable tables.
--
-- Namespaced `platform_` on purpose. A bare `set_updated_at()` is the obvious
-- name and therefore the name every other migration in this directory is
-- likely to reach for; two migrations authored concurrently that both CREATE
-- it collide on the way up, and — far worse — the first Down to run drops a
-- function the other tables' triggers still depend on. Prefixing makes this
-- function unambiguously owned by this migration, so its Down can drop it
-- without reaching outside its own blast radius.
--
-- If a shared `set_updated_at()` is later introduced in the bootstrap
-- migration, this one collapses into it in a follow-up migration. That is a
-- deliberate, reviewable change; a name collision is not.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION platform_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $platform_set_updated_at$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$platform_set_updated_at$;
-- +goose StatementEnd

COMMENT ON FUNCTION platform_set_updated_at() IS
    'BEFORE UPDATE trigger: stamps updated_at from the server clock. Namespaced to this migration so its Down cannot orphan another migration''s triggers.';

-- ---------------------------------------------------------------------------
-- audit_log
--
-- WHY THIS IS A HYPERTABLE
-- ------------------------
-- The prices table is the other hypertable in this schema (CLAUDE.md §4:
-- "Price ... Immutable; a new price is a new row. This is the hypertable"), and
-- the instruction was to coordinate rather than diverge. audit_log is the same
-- shape and gets the same treatment, for four reasons — the fourth is the one
-- that actually decided it:
--
--   1. It is genuinely time-series. Append-only, roughly monotonic in time,
--      never updated, and every real query against it is bounded by a time
--      range ("what happened to this market last night", "what did this admin
--      do in the last hour").
--
--   2. Retention becomes a constant-time operation. The deploy target is a
--      2 OCPU / 12 GB Oracle Ampere box that also runs Postgres, Redis, Kafka
--      and six Go services. On that machine `DELETE FROM audit_log WHERE
--      occurred_at < ...` is a bloat-and-autovacuum event; `drop_chunks` is a
--      `DROP TABLE` on one chunk.
--
--   3. One time-series mechanism in the system, not two. Timescale is already
--      a required dependency for prices; making audit_log a plain table would
--      mean hand-rolling partitioning or accepting an unbounded table.
--
--   4. **It strengthens append-only rather than weakening it.** The triggers
--      below make row-level UPDATE, DELETE and TRUNCATE impossible. Retention
--      still works anyway, because `drop_chunks` removes a whole chunk with
--      DROP TABLE — which does not fire row triggers. So the ONLY way to
--      remove audit history is to drop an entire, explicitly named time range:
--      a deliberate administrative act, auditable in its own right, and
--      impossible to do surgically. You cannot quietly delete the one row that
--      incriminates you; you can only drop the week it lived in. A plain table
--      would have to allow DELETE to be prunable at all, which is precisely
--      the capability an audit log must not have.
--
-- Cost accepted: a hypertable's PRIMARY KEY (and any UNIQUE index) must
-- include the partitioning column, so the key is (occurred_at, id) rather than
-- (id). `id` alone is a UUID and is unique in practice; nothing joins to
-- audit_log, so no foreign key needs the narrower key.
--
-- NO RETENTION POLICY IS INSTALLED HERE, on purpose. Silently deleting audit
-- history on a timer is a policy decision with legal shape, not a schema
-- default. The hypertable makes `add_retention_policy` *available*; choosing to
-- run it is a separate, reviewed migration.
--
-- WHY THERE IS NO FOREIGN KEY ON actor_id OR entity_id
-- ----------------------------------------------------
-- The frozen convention is "foreign keys everywhere the domain has a
-- relationship, with explicit ON DELETE behaviour chosen deliberately". The
-- deliberate choice here is NONE, twice over:
--
--   * `entity_type` / `entity_id` is a polymorphic reference — the target may
--     be a market, a wager, a user, a feature flag. Postgres has no FK for
--     that, and inventing one nullable FK column per entity type would add a
--     column to this table every time a new thing becomes auditable.
--
--   * `actor_id` deliberately does not reference users(id). Every available
--     ON DELETE behaviour is wrong for an audit trail: CASCADE erases the
--     record of what a user did when the user is deleted, which is the single
--     most important moment to still have it; RESTRICT means a user can never
--     be deleted; SET NULL anonymises the actor and destroys the trail's
--     value. An audit row must outlive its subject, so the actor is stored
--     denormalised, as the identity string that acted at that instant.
--
-- occurred_at VS created_at
-- -------------------------
--   occurred_at  when the action happened (event time). The partitioning
--                column, and the one every query and every Flink event-time
--                join in phase 12 uses.
--   created_at   when this row was written (ingestion time). Satisfies the
--                repo-wide created_at convention and, more usefully, makes
--                replay visible: a settle consumer replaying a Kafka partition
--                writes rows whose created_at is hours after occurred_at.
--
-- There is deliberately NO `CHECK (created_at >= occurred_at)`. occurred_at
-- comes from the acting service's clock and created_at from the database's;
-- a few milliseconds of skew between two containers would make that constraint
-- reject an audit write. See the atomicity note below for why rejecting an
-- audit write is unacceptable.
--
-- ATOMICITY CONTRACT (read this before writing the Go that inserts here)
-- ---------------------------------------------------------------------
-- The audit row MUST be inserted in the SAME transaction as the business
-- change it describes. Not in a defer, not on a queue, not best-effort. That
-- single rule is what makes this table trustworthy: it becomes impossible to
-- commit a state change without its audit record, and impossible to record an
-- audit entry for a change that rolled back.
--
-- It is also what makes the strict CHECK constraints below safe. A malformed
-- `action` does not silently lose an audit record — it aborts the business
-- transaction too, loudly, in the test suite, on the first run.
--
-- "Not editable by the same path that writes business data" is satisfied at
-- the operation level rather than the connection level: the writer's INSERT
-- must share the transaction, but the triggers below make UPDATE, DELETE and
-- TRUNCATE unavailable to that same connection — and to every other, including
-- the table owner.
-- ---------------------------------------------------------------------------
CREATE TABLE audit_log (
    -- Event time. Partitioning column; see the header note.
    occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Surrogate key, justified: an audit record has no provider-derived or
    -- domain-derived identity to carry — it is a fact this system minted. UUID
    -- rather than bigserial so the writer can generate it in Go *before* the
    -- INSERT (the same id then appears in the log line and the span attribute
    -- that reference this row), and because a sequence is a contention point
    -- on the highest-write-rate table in the schema.
    id           UUID        NOT NULL DEFAULT gen_random_uuid(),

    -- WHO. actor_id is never null: for actor_kind='system' it is the acting
    -- service name ('settle', 'ingest'), because an audit row with no actor at
    -- all answers none of the questions this table exists to answer.
    actor_kind   TEXT        NOT NULL,
    actor_id     TEXT        NOT NULL,

    -- WHAT. Dotted `domain.verb`: 'market.suspend', 'wager.place',
    -- 'feature_flag.update', 'auth.login'.
    action       TEXT        NOT NULL,

    -- TO WHAT. Polymorphic by design; see the header note on foreign keys.
    entity_type  TEXT        NOT NULL,
    entity_id    TEXT        NOT NULL,

    -- HOW IT WENT. A rejected action is at least as interesting as an accepted
    -- one — a run of failed admin actions is the signature this table exists
    -- to surface.
    outcome      TEXT        NOT NULL,

    -- Free-text justification. The admin console's "why are you suspending
    -- this market" box, and the rejection reason when outcome = 'failure'.
    reason       TEXT,

    -- BEFORE/AFTER. JSONB objects holding the changed fields only, not whole
    -- entity dumps — a diff, per the requirement. Both null is legitimate:
    -- 'auth.login' changes no persisted state.
    state_before JSONB,
    state_after  JSONB,

    -- TRACE CORRELATION. CLAUDE.md §9 wants "OpenTelemetry traces spanning
    -- ingest → pricer → stream" and "structured JSON logging via log/slog with
    -- trace correlation". These two columns are what make an audit row
    -- joinable to a Jaeger trace: they are W3C Trace Context ids, lowercase
    -- hex, exactly as Jaeger displays and searches them, and the CHECK below
    -- enforces that shape so "joinable to a trace" is a guarantee rather than
    -- an aspiration. Nullable because a startup-time or scheduled action may
    -- legitimately have no inbound trace.
    trace_id     TEXT,
    span_id      TEXT,

    -- Application-level request correlation, for the paths that have a request
    -- id but no sampled span.
    request_id   TEXT,

    -- PROVENANCE. client_ip is the only PII-bearing column in this file. It is
    -- kept because "who did this, from where" is the question a security audit
    -- actually asks, and INET (not TEXT) so v4/v6 comparison and subnet
    -- containment work. Any future data-retention or erasure policy has to
    -- reckon with this column specifically.
    client_ip    INET,
    user_agent   TEXT,

    -- Ingestion time. See the header note on occurred_at vs created_at.
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT audit_log_pkey PRIMARY KEY (occurred_at, id),

    -- Closed value set: structural to this table's own meaning.
    --   user    an end customer acting through the public API
    --   admin   an operator acting through the admin console (CLAUDE.md §6)
    --   system  an automated pipeline action with no human actor — settle
    --           grading a wager, ingest suspending a market on stale prices
    CONSTRAINT audit_log_actor_kind_check
        CHECK (actor_kind IN ('user', 'admin', 'system')),

    CONSTRAINT audit_log_outcome_check
        CHECK (outcome IN ('success', 'failure')),

    -- 128 bytes matches domain.MaxIDLen, so a value that a domain ID
    -- constructor accepts always fits here.
    CONSTRAINT audit_log_actor_id_check
        CHECK (actor_id <> '' AND length(actor_id) <= 128),

    -- Open value set, closed shape: dotted lowercase, at least one dot.
    CONSTRAINT audit_log_action_check
        CHECK (action ~ '^[a-z][a-z0-9_]*([.][a-z][a-z0-9_]*)+$' AND length(action) <= 96),

    CONSTRAINT audit_log_entity_type_check
        CHECK (entity_type ~ '^[a-z][a-z0-9_]*$' AND length(entity_type) <= 64),

    CONSTRAINT audit_log_entity_id_check
        CHECK (entity_id <> '' AND length(entity_id) <= 128),

    CONSTRAINT audit_log_reason_check
        CHECK (reason IS NULL OR (reason <> '' AND length(reason) <= 1024)),

    -- Objects, not arrays or bare scalars. A diff is a mapping of field to
    -- value; anything else here is a serialization bug at the call site.
    CONSTRAINT audit_log_state_before_check
        CHECK (state_before IS NULL OR jsonb_typeof(state_before) = 'object'),
    CONSTRAINT audit_log_state_after_check
        CHECK (state_after IS NULL OR jsonb_typeof(state_after) = 'object'),

    -- W3C Trace Context: 16-byte trace id and 8-byte span id, lowercase hex,
    -- and neither may be all zeroes (the spec's "invalid" sentinel).
    CONSTRAINT audit_log_trace_id_check
        CHECK (trace_id IS NULL OR (trace_id ~ '^[0-9a-f]{32}$' AND trace_id <> repeat('0', 32))),
    CONSTRAINT audit_log_span_id_check
        CHECK (span_id IS NULL OR (span_id ~ '^[0-9a-f]{16}$' AND span_id <> repeat('0', 16))),

    -- A span id without its trace id cannot be looked up in Jaeger, so it is
    -- not a partial record — it is a broken one.
    CONSTRAINT audit_log_span_requires_trace_check
        CHECK (span_id IS NULL OR trace_id IS NOT NULL),

    CONSTRAINT audit_log_request_id_check
        CHECK (request_id IS NULL OR (request_id <> '' AND length(request_id) <= 128)),

    CONSTRAINT audit_log_user_agent_check
        CHECK (user_agent IS NULL OR (user_agent <> '' AND length(user_agent) <= 512))
);

-- 7 days per chunk. An order of magnitude coarser than the prices hypertable
-- should be, and deliberately so: audit volume is bounded by human and
-- operator actions plus settlement events, not by odds ticks, so it is smaller
-- than prices by orders of magnitude. Timescale's guidance is to size a chunk
-- so the working set fits comfortably in memory; on a 12 GB box, thousands of
-- tiny daily chunks would cost more in planning overhead than they save.
--
-- create_default_indexes => FALSE because the default is an index on
-- (occurred_at DESC), and the primary key (occurred_at, id) already leads with
-- that column. Letting it be created would put a redundant index on the
-- highest-write-rate table in the schema.
SELECT create_hypertable(
    'audit_log',
    by_range('occurred_at', INTERVAL '7 days'),
    create_default_indexes => FALSE
);

-- Indexes, kept deliberately few. Every index here is a write-amplification
-- tax on the busiest append path in the system, so each one has to answer a
-- question the product actually asks:
--
--   entity  "show me everything that has happened to this market/wager/user"
--           — the admin console's entity timeline.
--   actor   "what has this admin done" / "what has this user done"
--           — the security review and the user's own activity history.
--   trace   "given this Jaeger trace, what did it change"
--           — the CLAUDE.md §9 trace-correlation story, in the direction that
--           is otherwise a full scan. Partial, because rows without a trace id
--           can never satisfy this query and should not be in the index.
--
-- An index on `action` alone is deliberately absent: every real query by
-- action is already scoped by entity, actor, or a time range, and the time
-- range is what the hypertable's chunk exclusion is for.
CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id, occurred_at DESC);
CREATE INDEX audit_log_actor_idx  ON audit_log (actor_kind, actor_id, occurred_at DESC);
CREATE INDEX audit_log_trace_idx  ON audit_log (trace_id, occurred_at DESC) WHERE trace_id IS NOT NULL;

-- ---------------------------------------------------------------------------
-- APPEND-ONLY ENFORCEMENT
--
-- An audit log that the application can edit is not evidence, it is a table
-- with a suggestive name. Enforcement lives in the database rather than in Go
-- because the property has to hold for `make psql`, for a stray sqlc query,
-- for a future service nobody has written yet, and for the operator at 2am —
-- not only for the code path that happens to be disciplined today.
--
-- Triggers, not privileges, are the mechanism. `REVOKE UPDATE, DELETE` is the
-- textbook answer and it is not sufficient here: the application connects as
-- the database owner, and an owner's privileges on its own table are
-- self-restorable — one GRANT and the protection is gone, silently. A trigger
-- fires for the owner and for a superuser alike. Removing it requires DROP
-- TRIGGER or ALTER TABLE ... DISABLE TRIGGER, which are schema changes: loud,
-- and visible in a schema diff.
--
-- Interaction with Timescale, verified rather than assumed: BEFORE ROW
-- triggers on a hypertable are propagated to every chunk, so the guard holds
-- on the chunks that actually store the rows, not just on the empty root.
-- `drop_chunks` bypasses all of this by design (DROP TABLE fires no row
-- trigger) — that is the retention escape hatch described in the header, and
-- it can only remove whole time ranges, never a chosen row.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION audit_log_append_only() RETURNS trigger
    LANGUAGE plpgsql
AS $audit_log_append_only$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only: % is not permitted', TG_OP
        USING ERRCODE = 'restrict_violation',
              HINT    = 'Audit history is evidence. Correct a wrong entry by INSERTing a '
                        'compensating record, never by editing or removing the original. '
                        'Bulk removal is a retention decision: drop_chunks() on a named time range.';
    RETURN NULL;
END;
$audit_log_append_only$;
-- +goose StatementEnd

COMMENT ON FUNCTION audit_log_append_only() IS
    'Guard trigger: raises on any UPDATE, DELETE or TRUNCATE against audit_log. SQLSTATE 23001 (restrict_violation).';

CREATE TRIGGER audit_log_no_update
    BEFORE UPDATE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

CREATE TRIGGER audit_log_no_delete
    BEFORE DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_append_only();

-- A row-level trigger does not fire for TRUNCATE, so the third guard is
-- statement-level and separate. Without it the two above are theatre: TRUNCATE
-- would remove every row while satisfying both.
CREATE TRIGGER audit_log_no_truncate
    BEFORE TRUNCATE ON audit_log
    FOR EACH STATEMENT EXECUTE FUNCTION audit_log_append_only();

COMMENT ON TABLE audit_log IS
    'Append-only forensic record of every state-changing action (CLAUDE.md §6). Hypertable partitioned on occurred_at. UPDATE/DELETE/TRUNCATE are blocked by trigger; rows are removable only by dropping a whole time chunk.';
COMMENT ON COLUMN audit_log.occurred_at  IS 'Event time: when the action happened, from the acting service''s clock. Partitioning column.';
COMMENT ON COLUMN audit_log.created_at   IS 'Ingestion time: when this row was written, from the database clock. Differs from occurred_at on replay.';
COMMENT ON COLUMN audit_log.actor_kind   IS 'user | admin | system. Must match a constant in internal/audit.';
COMMENT ON COLUMN audit_log.actor_id     IS 'Domain UserID for user/admin; acting service name for system. Denormalised with no FK so the record outlives its subject.';
COMMENT ON COLUMN audit_log.action       IS 'Dotted lowercase domain.verb, e.g. market.suspend, wager.place, feature_flag.update.';
COMMENT ON COLUMN audit_log.entity_type  IS 'Polymorphic target type, e.g. market, wager, feature_flag. No FK is possible.';
COMMENT ON COLUMN audit_log.outcome      IS 'success | failure. A rejected action is recorded, not discarded.';
COMMENT ON COLUMN audit_log.state_before IS 'JSONB object: changed fields before the action. Null when the action changes no persisted state.';
COMMENT ON COLUMN audit_log.state_after  IS 'JSONB object: changed fields after the action.';
COMMENT ON COLUMN audit_log.trace_id     IS 'W3C Trace Context trace id, 32 lowercase hex. Joins this row to a Jaeger trace (CLAUDE.md §9).';
COMMENT ON COLUMN audit_log.span_id      IS 'W3C Trace Context span id, 16 lowercase hex. Requires trace_id.';
COMMENT ON COLUMN audit_log.client_ip    IS 'Originating client address. The only PII-bearing column in this migration.';

-- ---------------------------------------------------------------------------
-- feature_flags
--
-- CLAUDE.md §6 "Platform": feature flags. Operator-controlled, low cardinality,
-- read constantly and written rarely — the read path belongs in a process-local
-- cache refreshed from here, never a per-request SELECT.
--
-- SEMANTICS OF enabled × rollout_percent — this is a contract the Go
-- implementation must honour exactly, and it is written down here because the
-- two columns are ambiguous unless someone fixes their interaction:
--
--   enabled = false  →  OFF for everyone, whatever rollout_percent says.
--                       This makes `enabled` an unambiguous kill switch: one
--                       UPDATE turns a feature off for 100% of traffic without
--                       destroying the rollout state you would need to resume.
--   enabled = true   →  ON for the bucketed rollout_percent fraction.
--                       rollout_percent = 100 means everyone.
--
-- Bucketing is deterministic and computed in Go, not here: hash(key ‖ user_id)
-- mod 100 < rollout_percent. Deterministic so a user does not flap between
-- variants across requests, and keyed by the flag as well as the user so two
-- flags at 10% do not select the same unlucky decile.
--
-- `targeting` is an open JSONB object for rules that are not a percentage
-- (allow-lists, environment pins). It is intentionally schemaless: the shape is
-- owned by the Go type that unmarshals it, and a column-per-rule design would
-- need a migration for every new rule kind.
--
-- NO ROWS ARE INSERTED. A flag that exists because a migration created it is
-- seed data, and the charter forbids it. Flags are created through the admin
-- console, which writes an audit_log row for the creation.
-- ---------------------------------------------------------------------------
CREATE TABLE feature_flags (
    -- Natural TEXT primary key per the frozen convention: the flag key IS the
    -- identity, it appears verbatim in Go call sites and in the admin console
    -- URL, and it must survive a database rebuild. A surrogate id here would
    -- be a second identifier for a thing that already has a perfectly good one.
    key             TEXT        NOT NULL,

    -- NOT NULL and non-empty on purpose. An undocumented flag is a flag nobody
    -- dares delete two years from now.
    description     TEXT        NOT NULL,

    enabled         BOOLEAN     NOT NULL DEFAULT FALSE,
    rollout_percent SMALLINT    NOT NULL DEFAULT 0,
    targeting       JSONB       NOT NULL DEFAULT '{}'::jsonb,

    -- Who last changed it. Denormalised identity string, same reasoning as
    -- audit_log.actor_id: no FK, because the record of who flipped a flag must
    -- survive the deletion of the account that flipped it. The full history of
    -- changes lives in audit_log; this column is the convenience answer to
    -- "who touched this last" without a join.
    updated_by      TEXT        NOT NULL,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT feature_flags_pkey PRIMARY KEY (key),

    -- Dotted lowercase, but unlike audit_log.action a single segment is legal:
    -- 'maintenance_mode' is a reasonable flag name, 'analytics.arb_scanner' is
    -- a better one.
    CONSTRAINT feature_flags_key_check
        CHECK (key ~ '^[a-z][a-z0-9_]*([.][a-z][a-z0-9_]*)*$' AND length(key) <= 96),

    CONSTRAINT feature_flags_description_check
        CHECK (description <> '' AND length(description) <= 512),

    CONSTRAINT feature_flags_rollout_percent_check
        CHECK (rollout_percent BETWEEN 0 AND 100),

    CONSTRAINT feature_flags_targeting_check
        CHECK (jsonb_typeof(targeting) = 'object'),

    CONSTRAINT feature_flags_updated_by_check
        CHECK (updated_by <> '' AND length(updated_by) <= 128)
);

CREATE TRIGGER feature_flags_set_updated_at
    BEFORE UPDATE ON feature_flags
    FOR EACH ROW EXECUTE FUNCTION platform_set_updated_at();

COMMENT ON TABLE feature_flags IS
    'Operator-controlled runtime switches (CLAUDE.md §6). Empty after migration by design; flags are created through the admin console, never seeded.';
COMMENT ON COLUMN feature_flags.enabled         IS 'Kill switch. FALSE means off for everyone regardless of rollout_percent.';
COMMENT ON COLUMN feature_flags.rollout_percent IS 'Percentage rollout, applied only when enabled is TRUE. Bucketing is hash(key || user_id) mod 100, computed in Go.';
COMMENT ON COLUMN feature_flags.targeting       IS 'Schemaless JSONB object for non-percentage targeting rules. Shape owned by the Go type that unmarshals it.';
COMMENT ON COLUMN feature_flags.updated_by      IS 'Identity that last changed the flag. Convenience denormalisation; the authoritative history is in audit_log.';

-- ---------------------------------------------------------------------------
-- market_suspensions
--
-- CLAUDE.md §6 "Platform": "admin console for market suspension". The market's
-- own lifecycle already has domain.MarketStatusSuspended — that column answers
-- "is this market suspended". This table answers the three questions the admin
-- console actually needs and a status enum cannot hold: WHO suspended it, WHY,
-- and WHEN it was lifted.
--
-- WHY THIS IS A SEPARATE TABLE AND NOT COLUMNS ON markets
-- ------------------------------------------------------
-- Two reasons, one structural and one about ownership:
--
--   * A market can be suspended, resumed, and suspended again — that is an
--     explicitly legal transition (domain.MarketStatus.CanTransitionTo allows
--     suspended → open). Columns on `markets` hold only the current episode
--     and overwrite the previous one; a market suspended three times in a
--     live game leaves no trace of the first two. This is a one-to-many
--     relationship and it needs its own table.
--   * `markets` is owned by the catalogue migration. Adding columns to another
--     agent's table is how concurrently authored schemas corrupt each other.
--
-- WHY NOT JUST QUERY audit_log
-- ----------------------------
-- Different access pattern, and the difference is real rather than stylistic.
-- audit_log is append-only forensic history: wide rows, JSONB diffs, no
-- uniqueness invariants, partitioned for time-range scans. This table is
-- current operational state read on a warm path ("this market is suspended —
-- line move") and it enforces an invariant audit_log structurally cannot: at
-- most one open suspension per market. An append-only log cannot express "at
-- most one", because expressing it would require the log to be mutable.
--
-- Both are written. The suspension row is the state; the audit row is the
-- evidence. They go in the same transaction.
-- ---------------------------------------------------------------------------
CREATE TABLE market_suspensions (
    -- Surrogate key, justified: a suspension episode has no provider-derived
    -- identity — it is a fact this system minted, exactly like an audit row.
    -- UUID over bigserial for the same reason as audit_log.id: the Go caller
    -- generates it before the INSERT so the audit row written in the same
    -- transaction can name it as its entity_id.
    id           UUID        NOT NULL DEFAULT gen_random_uuid(),

    -- ON DELETE CASCADE, chosen rather than defaulted. A suspension episode is
    -- a property OF a market, meaningless without it, and this table is
    -- operational state rather than evidence — so cascading is correct and
    -- loses nothing: the audit_log row recording the suspension carries no FK
    -- and survives the market's deletion. That split is the whole point of
    -- keeping the two tables separate. (RESTRICT was the alternative and is
    -- wrong: it would make a stale market undeletable forever because it was
    -- once suspended for thirty seconds.)
    market_id    TEXT        NOT NULL
        REFERENCES markets (id) ON DELETE CASCADE,

    -- Open value set, closed shape. Deliberately NOT a CHECK IN (...) list:
    -- there is no domain.SuspensionReason constant today, and minting the
    -- canonical list of reasons in SQL before the Go code names them would
    -- create exactly the second source of truth the TEXT-over-ENUM decision
    -- above exists to avoid. Expected values are 'line_move', 'stale_prices',
    -- 'provider_outage', 'injury_news', 'pricing_error', 'manual_review'.
    -- Closing the set is a follow-up migration, once the constants exist.
    reason       TEXT        NOT NULL,

    -- Operator's free-text elaboration. Optional; `reason` is the machine-
    -- readable part.
    note         TEXT,

    -- Actor identities, denormalised with no FK, same reasoning as
    -- audit_log.actor_id. 'system' is a legal value here: ingest suspends
    -- markets automatically when prices go stale, and that is not a human.
    suspended_by TEXT        NOT NULL,
    suspended_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Null while the suspension is open. The partial unique index below turns
    -- "lifted_at IS NULL" into the definition of "currently suspended".
    lifted_by    TEXT,
    lifted_at    TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT market_suspensions_pkey PRIMARY KEY (id),

    CONSTRAINT market_suspensions_reason_check
        CHECK (reason ~ '^[a-z][a-z0-9_]*$' AND length(reason) <= 64),

    CONSTRAINT market_suspensions_note_check
        CHECK (note IS NULL OR (note <> '' AND length(note) <= 1024)),

    CONSTRAINT market_suspensions_suspended_by_check
        CHECK (suspended_by <> '' AND length(suspended_by) <= 128),

    CONSTRAINT market_suspensions_lifted_by_check
        CHECK (lifted_by IS NULL OR (lifted_by <> '' AND length(lifted_by) <= 128)),

    -- Lifting is one event: you cannot record who lifted it without when, or
    -- when without who. Written as an equality of two null-tests so both
    -- half-states are rejected by one constraint.
    CONSTRAINT market_suspensions_lift_pair_check
        CHECK ((lifted_by IS NULL) = (lifted_at IS NULL)),

    -- Time cannot run backwards within a single episode. Unlike the
    -- occurred_at/created_at pair on audit_log, both of these timestamps are
    -- written by the same service in the same order, so there is no cross-
    -- container clock skew for this constraint to trip over.
    CONSTRAINT market_suspensions_lift_order_check
        CHECK (lifted_at IS NULL OR lifted_at >= suspended_at)
);

-- The invariant that makes this table a state store rather than a second log:
-- a market has at most one OPEN suspension. Without it, two admins clicking
-- "suspend" concurrently produce two open episodes and lifting one leaves the
-- market suspended by a ghost. A partial unique index enforces it in the
-- database, where a read-modify-write in Go cannot.
CREATE UNIQUE INDEX market_suspensions_one_open_idx
    ON market_suspensions (market_id) WHERE lifted_at IS NULL;

-- History for one market, newest first: the admin console's per-market panel.
CREATE INDEX market_suspensions_market_idx
    ON market_suspensions (market_id, suspended_at DESC);

CREATE TRIGGER market_suspensions_set_updated_at
    BEFORE UPDATE ON market_suspensions
    FOR EACH ROW EXECUTE FUNCTION platform_set_updated_at();

COMMENT ON TABLE market_suspensions IS
    'Admin-console market suspension episodes (CLAUDE.md §6): who, why, and whether still open. Current operational state; the immutable evidence is in audit_log.';
COMMENT ON COLUMN market_suspensions.market_id    IS 'FK to markets(id) ON DELETE CASCADE: this is state about a market, not evidence about it.';
COMMENT ON COLUMN market_suspensions.reason       IS 'Machine-readable reason code. Value set is open pending a domain.SuspensionReason constant; shape is constrained.';
COMMENT ON COLUMN market_suspensions.suspended_by IS 'Identity that suspended the market. May be a service name when ingest suspends automatically.';
COMMENT ON COLUMN market_suspensions.lifted_at    IS 'NULL means the suspension is open. Enforced unique per market by market_suspensions_one_open_idx.';

-- +goose Down

-- Reversibility, per CLAUDE.md §12: "Migrations are forward-only, and every one
-- is reversible in review before it is applied." This Down is the exact inverse
-- of the Up and is proven to run — it is the review artifact, not an
-- operational tool.
--
-- Read that distinction literally where audit_log is concerned. Running this
-- Down against a database holding real audit history DESTROYS THAT HISTORY,
-- and the append-only triggers above cannot stop it, because DROP TABLE is not
-- a row operation. That is not a hole in the design; it is the reason the
-- operational policy is forward-only. Roll this back on a throwaway database
-- during review, never on one that has served traffic.
--
-- Order matters: tables first (dropping a table drops its triggers), then the
-- functions those triggers depended on. Reversing that order fails with a
-- dependency error.

DROP TABLE IF EXISTS market_suspensions;
DROP TABLE IF EXISTS feature_flags;

-- DROP TABLE on a hypertable removes the hypertable and every chunk with it;
-- no drop_hypertable() call is needed or exists.
DROP TABLE IF EXISTS audit_log;

DROP FUNCTION IF EXISTS audit_log_append_only();
DROP FUNCTION IF EXISTS platform_set_updated_at();
