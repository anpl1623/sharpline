-- =============================================================================
-- 00005  accounts and auth
-- =============================================================================
--
-- CLAUDE.md section 6 (Account): "email/password auth with argon2id; JWT access
-- tokens plus rotating refresh tokens with reuse detection; optional TOTP 2FA;
-- play-money balance; responsible-gaming-style self-imposed limits".
--
-- This migration owns the identity half of that sentence. The play-money
-- balance is NOT here and never will be: CLAUDE.md section 4 states "Balances
-- are derived, never stored as a mutable field", so a user's balance is a fold
-- over ledger_entries (migration 00006) and there is no balance column in this
-- schema or any other.
--
--
-- WHAT IS DELIBERATELY ABSENT, AND MUST STAY ABSENT
-- -------------------------------------------------
-- CLAUDE.md section 0: this is "not a licensed sportsbook. No real money moves.
-- No KYC, no geolocation gating, no payment processing, no custody of funds."
--
-- So there is deliberately NO:
--
--   * ssn, tax id, passport or driver's licence number
--   * date of birth held for age verification (no date of birth at all)
--   * legal name, residential address, or any other KYC identity attribute
--   * payment instrument -- no card, no bank account, no wallet, no token
--   * geolocation column of any kind: no country, no state, no jurisdiction,
--     no last-seen IP held for gating, no device geo fingerprint
--   * document-upload or verification-status column
--
-- None of these are oversights and none of them are "TODO". Adding one turns a
-- rigorous simulation into an unlicensed real-money book, which CLAUDE.md
-- section 0 identifies as "a legal liability on a resume". If a future phase
-- believes it needs one of these columns, that is an ADR and a change to
-- CLAUDE.md section 0 first, and a migration second.
--
-- The one place an IP address legitimately appears is the audit log (CLAUDE.md
-- section 6, Platform: "audit log on every state-changing action"), which is not
-- this migration and is not owned here.
--
--
-- ENUM REPRESENTATION: TEXT + CHECK, EVERYWHERE
-- ----------------------------------------------
-- Every closed value set in this schema is `TEXT` constrained by a named CHECK,
-- not a PostgreSQL native ENUM type. The deciding argument is CLAUDE.md section
-- 12: "Migrations are forward-only, and every one is reversible in review before
-- it is applied."
--
--   * A CHECK constraint is reversible. Widening or narrowing a value set is
--     `ALTER TABLE ... DROP CONSTRAINT` + `ADD CONSTRAINT`, both of which run
--     inside the migration's transaction and both of which have an exact
--     inverse to write in the Down block.
--
--   * A native ENUM is not. `ALTER TYPE ... ADD VALUE` has no inverse -- there
--     is no `DROP VALUE` -- so removing a value means recreating the type and
--     rewriting every column that uses it. A migration that cannot be written in
--     reverse cannot be reviewed the way section 12 requires.
--
-- Two secondary reasons, neither decisive on its own. The domain already
-- serialises every enum as a lowercase string through MarshalText/ParseX (see
-- internal/domain), so TEXT is the identity mapping and needs no driver-side
-- type registration in pgx. And a CHECK is readable in `psql` without joining
-- pg_enum, which matters when the ledger is being audited by hand.
--
-- The cost paid is storage: a short TEXT is ~1 byte of header plus the string
-- against an ENUM's fixed 4. On these tables that is noise.
--
--
-- ENUM VALUES WITHOUT DOMAIN CONSTANTS -- READ THIS, PHASE 5
-- ----------------------------------------------------------
-- The project convention is that every enum value matches a constant in
-- internal/domain exactly. Four value sets in this file CANNOT satisfy that
-- today, because the domain deliberately models no user entity at all --
-- internal/domain/wager.go says so directly:
--
--     "The domain models no user entity: authentication, email, password
--      hashing and 2FA live in internal/auth (CLAUDE.md section 8)"
--
-- The value sets defined here with no Go counterpart yet are:
--
--     users.status                     active | suspended | self_excluded | closed
--     refresh_token_families.revoked_reason
--                                      logout | reuse_detected
--                                      | credential_change | operator
--     user_limits.kind                 grant | stake | loss | session
--     user_limits.period               day | week | month | session
--
-- Phase 5 (internal/auth) MUST define matching Go constants with String() /
-- ParseX() pairs producing exactly these lowercase spellings, the same way
-- internal/domain does for WagerStatus and EntryKind. Until then this file is
-- the single source of truth for them. Three of the four user_limits.kind values
-- are chosen to be identical to existing EntryKind spellings -- 'grant' is
-- EntryKindGrant, 'stake' is EntryKindStake, 'loss' is EntryKindLoss -- so that
-- enforcing a limit is a sum over ledger_entries filtered by the same string,
-- with no translation table in between.
--
--
-- NO TRIGGER ANYWHERE IN THIS SCHEMA EVER WRITES A DOMAIN INSTANT
-- ---------------------------------------------------------------
-- An earlier revision of this header claimed something stronger and different:
-- "NO updated_at TRIGGER, ANYWHERE IN THIS SCHEMA". That claim was false about
-- the schema it appeared in, and the falsehood was load-bearing -- 00001, 00002
-- and 00006 each cite it, and 00001 declined to create a shared trigger
-- function partly because of it. 00002 installs six `updated_at` triggers
-- (sports, leagues, books, events, markets, selections), 00006 installs two
-- (wagers, legs), and 00007 installs two (feature_flags, market_suspensions).
-- Meanwhile this file's own five tables carried an `updated_at` column with no
-- trigger, so one column name meant "server clock at last UPDATE" on ten tables
-- and "whatever the application last wrote" on five. That is the actual defect,
-- and it is resolved here rather than reported again.
--
-- THE INVARIANT, stated so it is checkable:
--
--   A trigger may stamp `updated_at`, which is row bookkeeping and means
--   nothing to the domain. No trigger may write a column that carries a
--   DOMAIN instant -- an instant the application computed and must be able to
--   reproduce.
--
-- THE REDELIVERY ARGUMENT IS UNCHANGED AND STILL BINDING. The domain's state
-- transitions take the instant as an explicit parameter
-- (Wager.Settle(status, amount, at), Leg.WithStatus(status, at)) precisely so
-- that a redelivered Kafka message re-applies the ORIGINAL instant rather than
-- the wall clock. A trigger overwriting that with now() would silently discard
-- the value the domain worked to preserve. The database is not the clock for
-- any of those columns. 00002 answered this by splitting the two meanings into
-- two columns -- `observed_at` for the provider instant, `updated_at` for row
-- bookkeeping -- and triggering only the bookkeeping one. 00006 adopted that
-- resolution explicitly. This file now adopts it too, which is what makes the
-- three cross-references coherent.
--
-- PHASE 3, THIS IS THE HALF THAT CONSTRAINS YOU: `auth_set_updated_at()` below
-- touches `updated_at` and nothing else. Every domain instant in this file --
-- `users.password_changed_at`, `user_totp.confirmed_at`,
-- `refresh_token_families.started_at` / `revoked_at`, `refresh_tokens.issued_at`
-- / `expires_at` / `used_at`, `user_limits.requested_at` / `effective_from` /
-- `superseded_at` -- is written by the application from the value it was given,
-- never by the database, and a redelivered message therefore re-applies the
-- same instant and produces the same row. If a future trigger needs to touch
-- one of those columns, the answer is a new column, not a wider trigger.
--
-- +goose Up

-- -----------------------------------------------------------------------------
-- Shared trigger function: maintain updated_at on this migration's tables.
--
-- Namespaced `auth_` for the reason 00007 spells out about
-- `platform_set_updated_at()` and 00002 about `catalogue_set_updated_at()`: a
-- bare `set_updated_at()` is the name every concurrently-authored migration
-- reaches for, two of them collide on the way up, and the first Down to run
-- drops a function another migration's triggers still need. The prefix makes
-- the blast radius of this file's Down exactly this file. This file neither
-- creates nor drops any sibling's function.
--
-- It stamps `updated_at` and nothing else, and it must never be extended. Every
-- other timestamp in this file carries a domain instant the application must be
-- able to reproduce on a Kafka redelivery -- see the header block above.
-- -----------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION auth_set_updated_at() RETURNS trigger
    LANGUAGE plpgsql
AS $auth_set_updated_at$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$auth_set_updated_at$;
-- +goose StatementEnd

COMMENT ON FUNCTION auth_set_updated_at() IS
    'BEFORE UPDATE trigger: stamps updated_at from the server clock. Never touches a domain instant (password_changed_at, confirmed_at, started_at, revoked_at, issued_at, expires_at, used_at, requested_at, effective_from, superseded_at). Namespaced to this migration so its Down cannot orphan another migration''s triggers.';

-- -----------------------------------------------------------------------------
-- users
-- -----------------------------------------------------------------------------
-- The primary key is TEXT carrying domain.UserID, not a bigserial. Every
-- identifier in this system is provider-derived or externally-visible and must
-- survive a rebuild of the row it names; a surrogate integer would have to be
-- kept in sync with the real identity anyway. The CHECK reproduces
-- domain.validID exactly (see internal/domain/ids.go): 1 to MaxIDLen=128 bytes
-- of [A-Za-z0-9._-], with ':' excluded because CLAUDE.md section 5 defines
-- WebSocket channels as `event:{id}` and a colon inside an identifier would make
-- splitting a channel name ambiguous.
CREATE TABLE users (
    id              TEXT        PRIMARY KEY
                                CONSTRAINT users_id_charset
                                CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- Email is stored ALREADY NORMALISED to lowercase, and the CHECK makes that
    -- a database invariant rather than an application convention.
    --
    -- The alternative designs were both worse. `citext` needs a non-core
    -- extension for one column and hides a locale-dependent comparison behind an
    -- `=` that looks ordinary. A plain TEXT column with a `UNIQUE (lower(email))`
    -- functional index lets 'A@x.com' and 'a@x.com' both exist in the stored
    -- column, and forces every lookup to remember the lower() wrapper -- forget
    -- it once and the query silently misses the row. Normalising on the way in
    -- gives a plain b-tree unique index that an exact-match lookup uses directly,
    -- and makes the un-normalised row unstorable.
    --
    -- lower(text) is IMMUTABLE, so it is legal inside a CHECK.
    --
    -- The shape check is deliberately permissive: one '@' with non-empty,
    -- whitespace-free sides. Tighter regexes reject deliverable addresses
    -- (quoted local parts, single-label domains) and the only real proof an
    -- address exists is sending mail to it.
    email           TEXT        NOT NULL UNIQUE
                                CONSTRAINT users_email_normalised
                                CHECK (email = lower(email))
                                CONSTRAINT users_email_length
                                CHECK (length(email) BETWEEN 3 AND 254)
                                CONSTRAINT users_email_shape
                                CHECK (email ~ '^[^[:space:]@]+@[^[:space:]@]+$'),

    -- The FULL PHC-format argon2id hash string, exactly as the hasher emitted
    -- it:
    --
    --     $argon2id$v=19$m=65536,t=3,p=4$<b64 salt>$<b64 digest>
    --
    -- One column, not five. The PHC string already carries the algorithm, the
    -- version, the memory/time/parallelism parameters and the salt, so splitting
    -- them into columns creates four opportunities to reassemble them wrongly
    -- and no capability. It also makes a parameter bump a pure application
    -- change: a hash written under the old cost verifies fine, and the
    -- application rehashes on the next successful login. No migration.
    --
    -- The CHECK is a real control, not decoration. It makes it structurally
    -- impossible to store a plaintext password, a reversibly-encrypted one, a
    -- bcrypt hash, or an argon2i/argon2d hash in this column: none of them start
    -- with the argon2id PHC prefix. CLAUDE.md section 6 says argon2id, and this
    -- is the schema refusing anything else.
    password_hash   TEXT        NOT NULL
                                CONSTRAINT users_password_hash_is_argon2id
                                CHECK (password_hash LIKE '$argon2id$%')
                                CONSTRAINT users_password_hash_length
                                CHECK (length(password_hash) BETWEEN 40 AND 512),

    -- When the credential last changed. Every refresh-token family issued before
    -- this instant is invalid by definition, which turns "log out everywhere on
    -- password change" into a comparison rather than a table scan and a bulk
    -- update.
    password_changed_at TIMESTAMPTZ NOT NULL,

    status          TEXT        NOT NULL DEFAULT 'active'
                                CONSTRAINT users_status_defined
                                CHECK (status IN ('active', 'suspended', 'self_excluded', 'closed')),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  users IS
    'A customer. Carries identity and credentials only: no KYC, no geolocation, no payment instrument, and no balance (balances are derived from ledger_entries).';
COMMENT ON COLUMN users.email IS
    'Lowercase-normalised address. The normalisation is enforced by CHECK, so a plain UNIQUE index is a correct case-insensitive uniqueness constraint.';
COMMENT ON COLUMN users.password_hash IS
    'Full PHC-format argon2id hash. Never a plaintext or reversibly-encrypted password; the CHECK makes anything but argon2id unstorable.';

-- `users` is mutable: status changes, and a credential change rewrites
-- password_hash and password_changed_at. The trigger stamps `updated_at` only;
-- `password_changed_at` is the application's value and stays untouched, because
-- "every refresh-token family issued before this instant is invalid" has to be
-- comparable against the instant the credential actually changed.
CREATE TRIGGER users_set_updated_at
    BEFORE UPDATE ON users
    FOR EACH ROW EXECUTE FUNCTION auth_set_updated_at();

-- -----------------------------------------------------------------------------
-- user_totp -- CREDENTIAL MATERIAL. HANDLE ACCORDINGLY.
-- -----------------------------------------------------------------------------
-- CLAUDE.md section 6: "optional TOTP 2FA". Optional is why this is a separate
-- table with a nullable relationship rather than nullable columns on `users`.
--
-- It is separate for a second, larger reason. A TOTP shared secret is a
-- BEARER CREDENTIAL: anyone holding it can mint valid codes forever. It is not
-- like a password hash, which is one-way. Keeping it out of `users` means the
-- routine `SELECT * FROM users` that every handler ends up writing cannot
-- accidentally load it, log it, or serialise it into a response.
--
-- REQUIREMENTS ON PHASE 5 (internal/auth). These are not suggestions:
--
--   1. The secret is stored ENCRYPTED AT REST and never in plaintext, never in
--      base32, and never in a URI. `secret_ciphertext` holds AEAD output
--      (AES-256-GCM or XChaCha20-Poly1305) under a key that lives in a
--      Kubernetes Secret / .env and NEVER in the database. A column holding the
--      raw secret would mean a single SELECT-only leak is a permanent, silent
--      full 2FA bypass for every user at once.
--
--   2. `key_id` names which key encrypted the row, so a key rotation is a
--      re-encrypt pass rather than a forced 2FA reset for every user. Do not
--      drop this column because there is only one key today.
--
--   3. The AEAD's additional authenticated data MUST include user_id, so a row
--      copied from one user to another fails to decrypt rather than silently
--      granting the attacker's device the victim's second factor.
--
--   4. This table is never included in a `SELECT *` join against users, is
--      never returned by any API handler in any shape, and is redacted from
--      logs. Consider a dedicated database role in phase 10 whose grants stop
--      at the table boundary.
--
--   5. `confirmed_at` is NULL between "the user scanned the QR code" and "the
--      user proved a code from it". An unconfirmed row must NOT be treated as
--      an enrolled second factor -- otherwise a mis-scan locks the account out.
CREATE TABLE user_totp (
    user_id             TEXT        PRIMARY KEY
                                    REFERENCES users (id) ON DELETE CASCADE,

    -- AEAD ciphertext of the TOTP shared secret. NEVER the secret itself.
    secret_ciphertext   BYTEA       NOT NULL
                                    CONSTRAINT user_totp_ciphertext_nonempty
                                    CHECK (octet_length(secret_ciphertext) > 0),

    -- AEAD nonce. Unique per encryption under a given key; reusing one with
    -- GCM is a catastrophic, silent break.
    secret_nonce        BYTEA       NOT NULL
                                    CONSTRAINT user_totp_nonce_nonempty
                                    CHECK (octet_length(secret_nonce) BETWEEN 12 AND 32),

    -- Which key encrypted this row. Enables rotation without a 2FA reset.
    key_id              TEXT        NOT NULL
                                    CONSTRAINT user_totp_key_id_shape
                                    CHECK (key_id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- NULL until the user proves a code. An unconfirmed row is not an enrolled
    -- second factor.
    confirmed_at        TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE  user_totp IS
    'TOTP second-factor enrolment. CREDENTIAL MATERIAL: the secret is stored as AEAD ciphertext under a key held outside the database, and this table is never selected by default, never returned by an API, and redacted from logs.';
COMMENT ON COLUMN user_totp.secret_ciphertext IS
    'AEAD ciphertext of the shared secret. Storing the raw secret here would make one SELECT leak a permanent full 2FA bypass for every enrolled user.';

-- Mutable: enrolment is confirmed, and the secret is re-minted on re-enrolment.
CREATE TRIGGER user_totp_set_updated_at
    BEFORE UPDATE ON user_totp
    FOR EACH ROW EXECUTE FUNCTION auth_set_updated_at();

-- ON DELETE CASCADE is correct here and nowhere else in this file: the TOTP row
-- is a property OF the user, has no independent meaning, and must not outlive
-- them. Every other reference to users below uses RESTRICT, because auth history
-- and money movements must survive.

-- -----------------------------------------------------------------------------
-- refresh_token_families -- the lineage half of rotation + reuse detection
-- -----------------------------------------------------------------------------
-- CLAUDE.md section 6 asks for "rotating refresh tokens with reuse detection".
-- That is a specific mechanism, not a flag, and it needs two tables:
--
--   * Rotation: redeeming refresh token N invalidates it and issues N+1. At any
--     instant a family has exactly one live token.
--
--   * Reuse detection: if a token that has ALREADY been redeemed is presented
--     again, that is either a replay by an attacker who stole it or a replay by
--     the legitimate client whose newer token the attacker stole. Both mean the
--     lineage is compromised and neither party can be told apart, so the ONLY
--     safe response is to revoke the WHOLE FAMILY -- every descendant of the
--     original login -- and force a fresh authentication.
--
-- "The whole family" is why this table exists. Without a lineage there is
-- nothing to revoke: revoking just the replayed token leaves the attacker's
-- freshly-rotated successor working.
CREATE TABLE refresh_token_families (
    id              TEXT        PRIMARY KEY
                                CONSTRAINT refresh_token_families_id_charset
                                CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- RESTRICT, not CASCADE: a login lineage is auth history. Deleting a user
    -- with live sessions must be an explicit, ordered operation, not a silent
    -- side effect.
    user_id         TEXT        NOT NULL
                                REFERENCES users (id) ON DELETE RESTRICT,

    -- When the family was born, i.e. when the user actually authenticated with
    -- their password (and second factor, if enrolled). Distinct from any
    -- individual token's issued_at, and it is the value an absolute session
    -- lifetime is measured from -- otherwise a lineage rotated every ten minutes
    -- lives forever.
    started_at      TIMESTAMPTZ NOT NULL,

    -- NULL means live. Non-NULL means every token in the lineage is dead
    -- regardless of its own used_at/expires_at.
    revoked_at      TIMESTAMPTZ,
    revoked_reason  TEXT
                                CONSTRAINT refresh_token_families_revoked_reason_defined
                                CHECK (revoked_reason IN ('logout', 'reuse_detected', 'credential_change', 'operator')),

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A revocation always carries its reason and a reason never appears without
    -- one. Without the biconditional, "how many sessions did we kill for token
    -- reuse this week" -- the only signal that tells you an attack happened --
    -- becomes unanswerable.
    CONSTRAINT refresh_token_families_revocation_complete
        CHECK ((revoked_at IS NULL) = (revoked_reason IS NULL)),
    CONSTRAINT refresh_token_families_revoked_after_start
        CHECK (revoked_at IS NULL OR revoked_at >= started_at)
);

COMMENT ON TABLE refresh_token_families IS
    'One login lineage. Reuse of any already-redeemed token in the lineage revokes the entire family, because the legitimate client and the thief cannot be distinguished.';

-- Mutable exactly once, when the family is revoked. `revoked_at` is the domain's
-- instant -- the moment reuse was detected, which a redelivered detection event
-- must re-apply identically -- so the trigger stamps `updated_at` and leaves it.
CREATE TRIGGER refresh_token_families_set_updated_at
    BEFORE UPDATE ON refresh_token_families
    FOR EACH ROW EXECUTE FUNCTION auth_set_updated_at();

-- -----------------------------------------------------------------------------
-- refresh_tokens
-- -----------------------------------------------------------------------------
-- Each row is one link in a family's chain. The chain is explicit
-- (parent_id -> the token this one replaced) rather than implied by ordering on
-- issued_at, because a timestamp comparison is not a proof of succession and
-- two tokens issued in the same millisecond would make the lineage ambiguous.
--
-- HOW REUSE DETECTION ACTUALLY WORKS, and what the database guarantees:
--
--   present token T:
--     1. look up by token_hash (never by the token itself -- see below)
--     2. if not found                    -> reject
--     3. if T.family is revoked          -> reject
--     4. if T.expires_at <= now()        -> reject
--     5. if T.used_at IS NOT NULL        -> REUSE. Revoke the whole family with
--                                           reason 'reuse_detected'. Reject.
--     6. otherwise: set T.used_at = now() and INSERT the successor with
--        parent_id = T.id, in ONE transaction.
--
-- Step 5 is a read. On its own it is racy: two concurrent presentations of the
-- same unused token can both pass it and both mint a successor, which is exactly
-- the attack. The partial unique index `refresh_tokens_one_successor` closes
-- that race in the database -- a token can have AT MOST ONE child, so the second
-- concurrent rotation fails on a unique violation whatever the application
-- believed it had read. Reuse is therefore structurally detectable, not
-- detectable-if-the-code-remembers-to-look.
CREATE TABLE refresh_tokens (
    -- The token's public identifier -- the `jti` if these are carried as JWTs.
    -- Safe to log. This is NOT the secret.
    id              TEXT        PRIMARY KEY
                                CONSTRAINT refresh_tokens_id_charset
                                CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    family_id       TEXT        NOT NULL
                                REFERENCES refresh_token_families (id) ON DELETE RESTRICT,

    -- The token this one replaced. NULL exactly on the family's root token,
    -- which is the one minted by the original password login.
    --
    -- The FK is deliberately RESTRICT: the chain is the audit trail of a
    -- lineage, and deleting a link in the middle of it would leave the
    -- succession unprovable.
    parent_id       TEXT
                                CONSTRAINT refresh_tokens_not_own_parent
                                CHECK (parent_id IS NULL OR parent_id <> id),

    -- SHA-256 of the token secret. THE SECRET ITSELF IS NEVER STORED.
    --
    -- A refresh token is a bearer credential, so a database leak must not hand
    -- the attacker working tokens. It is hashed rather than argon2id'd on
    -- purpose: unlike a password it is high-entropy random bytes generated by
    -- us, so there is no dictionary to attack and no reason to pay a memory-hard
    -- KDF on the hot refresh path. That reasoning holds ONLY because the token
    -- is high-entropy -- phase 5 must generate it from crypto/rand with at least
    -- 256 bits, never from a counter, a UUIDv4, or a timestamp.
    token_hash      BYTEA       NOT NULL UNIQUE
                                CONSTRAINT refresh_tokens_hash_is_sha256
                                CHECK (octet_length(token_hash) = 32),

    issued_at       TIMESTAMPTZ NOT NULL,
    expires_at      TIMESTAMPTZ NOT NULL,

    -- Set exactly once, when the token is redeemed for its successor. A
    -- non-NULL used_at on a presented token IS the reuse signal.
    used_at         TIMESTAMPTZ,

    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT refresh_tokens_expires_after_issue
        CHECK (expires_at > issued_at),
    CONSTRAINT refresh_tokens_used_after_issue
        CHECK (used_at IS NULL OR used_at >= issued_at),

    -- Lets the composite FK below reference (id, family_id).
    CONSTRAINT refresh_tokens_id_family_key UNIQUE (id, family_id),

    -- A token's parent is always in the SAME family. Without this a rotation bug
    -- could graft one lineage onto another, and revoking the compromised family
    -- would then leave the attacker's branch alive under a different family id
    -- -- silently defeating the entire control.
    CONSTRAINT refresh_tokens_parent_same_family
        FOREIGN KEY (parent_id, family_id)
        REFERENCES refresh_tokens (id, family_id) ON DELETE RESTRICT
);

COMMENT ON TABLE refresh_tokens IS
    'One link in a rotation chain. Stores only a SHA-256 of the token secret; the secret itself never reaches the database.';
COMMENT ON COLUMN refresh_tokens.used_at IS
    'Set once, at redemption. A presented token with used_at already set is a reuse and must revoke the entire family.';
COMMENT ON COLUMN refresh_tokens.parent_id IS
    'The token this one replaced. NULL only on a family root. At most one child per parent -- see refresh_tokens_one_successor.';

-- The one legal mutation on this table is the once-only NULL -> non-NULL
-- transition on used_at, "plus updated_at" -- which is what this trigger writes,
-- and the only reason `updated_at` moves here at all.
--
-- Trigger firing order is deliberate and not accidental: PostgreSQL fires
-- same-event row triggers in name order, and `refresh_tokens_append_only` sorts
-- before `refresh_tokens_set_updated_at`, so an illegal edit is refused before
-- anything is stamped. The order does not actually matter for correctness --
-- refresh_tokens_assert_append_only() compares an explicit column list that
-- excludes updated_at, so it neither sees nor cares about the stamp -- but the
-- names should not be changed in a way that makes the stamp run first.
CREATE TRIGGER refresh_tokens_set_updated_at
    BEFORE UPDATE ON refresh_tokens
    FOR EACH ROW EXECUTE FUNCTION auth_set_updated_at();

-- THE structural half of reuse detection. A token may be rotated at most once,
-- enforced by the database rather than by a read-then-write in the application,
-- so two concurrent redemptions of one token cannot both succeed.
CREATE UNIQUE INDEX refresh_tokens_one_successor
    ON refresh_tokens (parent_id)
    WHERE parent_id IS NOT NULL;

-- A family has exactly one root: the token minted by the original login.
CREATE UNIQUE INDEX refresh_tokens_one_root
    ON refresh_tokens (family_id)
    WHERE parent_id IS NULL;

-- The lookup on every refresh is by hash; UNIQUE on token_hash already indexes
-- it. These two serve the other two access patterns: enumerate a family (to
-- revoke it), and sweep expired rows.
CREATE INDEX refresh_tokens_family_idx
    ON refresh_tokens (family_id, issued_at DESC);
CREATE INDEX refresh_tokens_expiry_idx
    ON refresh_tokens (expires_at);

CREATE INDEX refresh_token_families_user_idx
    ON refresh_token_families (user_id, started_at DESC);
-- Finding a user's live sessions is the common query; the revoked ones are
-- history and are not worth indexing.
CREATE INDEX refresh_token_families_live_idx
    ON refresh_token_families (user_id)
    WHERE revoked_at IS NULL;

-- -----------------------------------------------------------------------------
-- refresh token immutability
-- -----------------------------------------------------------------------------
-- Rotation is only a security control if the record of it cannot be edited. If
-- `used_at` can be cleared, reuse detection is bypassable with one UPDATE; if
-- `token_hash` can be rewritten, a revoked lineage can be resurrected under a
-- new secret. So the only mutation this table accepts is the once-only
-- NULL -> non-NULL transition on used_at, plus updated_at.
--
-- Deletion is permitted and is the intended way to expire old rows: expired,
-- fully-superseded links carry no security value once the family is closed. The
-- RESTRICT foreign keys force a sweep to delete a chain from the leaf backwards,
-- which is what keeps a half-deleted lineage from existing.
-- +goose StatementBegin
CREATE FUNCTION refresh_tokens_assert_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.family_id, NEW.parent_id, NEW.token_hash, NEW.issued_at, NEW.expires_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.family_id, OLD.parent_id, OLD.token_hash, OLD.issued_at, OLD.expires_at, OLD.created_at)
    THEN
        RAISE EXCEPTION
            'refresh token % is immutable once issued; only used_at may be set',
            OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at THEN
        RAISE EXCEPTION
            'refresh token % was already redeemed at %; used_at is write-once',
            OLD.id, OLD.used_at
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER refresh_tokens_append_only
    BEFORE UPDATE ON refresh_tokens
    FOR EACH ROW EXECUTE FUNCTION refresh_tokens_assert_append_only();

-- -----------------------------------------------------------------------------
-- user_limits -- responsible-gaming self-imposed limits
-- -----------------------------------------------------------------------------
-- CLAUDE.md section 6: "responsible-gaming-style self-imposed limits (a nod to
-- how the real domain works)".
--
-- WHY THERE IS NO 'deposit' LIMIT
--
-- A real book's headline self-imposed limit is a deposit cap. This system has no
-- deposits: CLAUDE.md section 0 rules out payment processing and custody of
-- funds entirely. Naming a limit 'deposit' would create a control that can never
-- fire and would invite a future contributor to build the deposit flow section 0
-- forbids. The play-money analogue is EntryKindGrant -- the only way money
-- enters a user's balance -- so the kind is called 'grant' and binds to the
-- ledger entry kind of the same name.
--
-- This is the same reasoning internal/domain/ledger.go gives for having no
-- bonus or promo account: "An account kind with no transaction that writes to it
-- is a column that will be wrong the first time somebody finally uses it."
--
-- THE FOUR KINDS, and how each is evaluated (all against migration 00006):
--
--   grant    sum of ledger_entries.amount_minor where kind='grant' and the
--            account is the user's cash, within the period
--   stake    sum of stake_minor over wagers placed within the period
--   loss     net of ledger entries against the user's cash within the period
--   session  wall-clock duration of a single authenticated session
--
-- WHY THE TABLE IS A HISTORY AND NOT A SETTINGS ROW
--
-- The real-world control is asymmetric: TIGHTENING a limit takes effect
-- immediately, LOOSENING it only after a cooling-off period. A single mutable
-- settings row cannot express "the user asked to raise their loss limit on
-- Tuesday; it becomes effective on Friday", and cannot answer "what limit was in
-- force when this wager was accepted" -- which is the only question that matters
-- if a customer later disputes it.
--
-- So rows are append-only history. `requested_at` records when the user asked,
-- `effective_from` when it binds (equal for a tightening, later for a
-- loosening), and `superseded_at` closes the row when a newer one replaces it.
-- The cooling-off DURATION is policy and lives in the betting service; the
-- schema's job is to make both instants permanent and auditable.
CREATE TABLE user_limits (
    id                  TEXT        PRIMARY KEY
                                    CONSTRAINT user_limits_id_charset
                                    CHECK (id ~ '^[A-Za-z0-9._-]{1,128}$'),

    -- RESTRICT: a responsible-gaming record outlives account closure. Deleting a
    -- self-excluded user's limit history is exactly the thing that must not
    -- happen by accident.
    user_id             TEXT        NOT NULL
                                    REFERENCES users (id) ON DELETE RESTRICT,

    kind                TEXT        NOT NULL
                                    CONSTRAINT user_limits_kind_defined
                                    CHECK (kind IN ('grant', 'stake', 'loss', 'session')),

    -- 'period' rather than 'window': WINDOW is a reserved word in PostgreSQL and
    -- would need quoting at every call site forever.
    period              TEXT        NOT NULL
                                    CONSTRAINT user_limits_period_defined
                                    CHECK (period IN ('day', 'week', 'month', 'session')),

    -- Money limits only. BIGINT minor units, per CLAUDE.md section 12: "All
    -- money and stake values are integer minor units." The upper bound is
    -- domain.MaxSafeMoney (2^53-1), which is both the largest integer float64
    -- holds exactly and JavaScript's Number.MAX_SAFE_INTEGER, so the value
    -- survives the trip to the Next.js frontend as a JSON number.
    amount_minor        BIGINT
                                    CONSTRAINT user_limits_amount_range
                                    CHECK (amount_minor IS NULL
                                           OR (amount_minor > 0 AND amount_minor <= 9007199254740991)),

    -- Session limit only. One day is the ceiling; a "session limit" longer than
    -- that is not a limit.
    duration_seconds    INTEGER
                                    CONSTRAINT user_limits_duration_range
                                    CHECK (duration_seconds IS NULL
                                           OR (duration_seconds > 0 AND duration_seconds <= 86400)),

    requested_at        TIMESTAMPTZ NOT NULL,
    effective_from      TIMESTAMPTZ NOT NULL,

    -- NULL means this is the current limit for (user_id, kind, period).
    superseded_at       TIMESTAMPTZ,

    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- The three biconditionals below make every impossible combination
    -- unstorable: a session limit denominated in money, a loss limit denominated
    -- in seconds, or a 'session' period on a money kind.
    CONSTRAINT user_limits_session_period
        CHECK ((kind = 'session') = (period = 'session')),
    CONSTRAINT user_limits_session_is_duration
        CHECK ((kind = 'session') = (duration_seconds IS NOT NULL)),
    CONSTRAINT user_limits_money_is_amount
        CHECK ((kind = 'session') = (amount_minor IS NULL)),

    -- A limit cannot bind before it was asked for. Equality is a tightening
    -- (immediate); a later effective_from is a loosening serving its cooling-off.
    CONSTRAINT user_limits_effective_after_request
        CHECK (effective_from >= requested_at),
    CONSTRAINT user_limits_superseded_after_request
        CHECK (superseded_at IS NULL OR superseded_at >= requested_at)
);

COMMENT ON TABLE user_limits IS
    'Append-only history of self-imposed responsible-gaming limits. The current limit for a (user, kind, period) is the row with superseded_at IS NULL.';
COMMENT ON COLUMN user_limits.effective_from IS
    'When the limit binds. Equal to requested_at for a tightening; later for a loosening serving its cooling-off period.';

-- Mutable exactly once, when the row is superseded. `superseded_at`,
-- `requested_at` and `effective_from` are all domain instants -- a customer must
-- be able to show what they set and when -- so the trigger stamps `updated_at`
-- and nothing else. As with refresh_tokens, `user_limits_append_only` sorts
-- before this trigger's name and compares a column list that excludes
-- updated_at.
CREATE TRIGGER user_limits_set_updated_at
    BEFORE UPDATE ON user_limits
    FOR EACH ROW EXECUTE FUNCTION auth_set_updated_at();

-- Exactly one current limit per (user, kind, period). Superseded rows are
-- unconstrained, which is what makes the history a history.
CREATE UNIQUE INDEX user_limits_current_key
    ON user_limits (user_id, kind, period)
    WHERE superseded_at IS NULL;

CREATE INDEX user_limits_user_idx
    ON user_limits (user_id, requested_at DESC);

-- -----------------------------------------------------------------------------
-- user_limits immutability
-- -----------------------------------------------------------------------------
-- A self-imposed limit that can be quietly edited is not a control. The only
-- legal mutation is closing the row (superseded_at NULL -> non-NULL), and rows
-- are never deleted: a customer must be able to show what they set and when,
-- and an operator must not be able to make that disappear.
-- +goose StatementBegin
CREATE FUNCTION user_limits_assert_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION
            'user_limits is append-only: responsible-gaming history cannot be deleted'
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF (NEW.id, NEW.user_id, NEW.kind, NEW.period, NEW.amount_minor,
        NEW.duration_seconds, NEW.requested_at, NEW.effective_from, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.user_id, OLD.kind, OLD.period, OLD.amount_minor,
        OLD.duration_seconds, OLD.requested_at, OLD.effective_from, OLD.created_at)
    THEN
        RAISE EXCEPTION
            'user limit % is immutable; supersede it with a new row instead of editing it',
            OLD.id
            USING ERRCODE = 'restrict_violation';
    END IF;

    IF OLD.superseded_at IS NOT NULL AND NEW.superseded_at IS DISTINCT FROM OLD.superseded_at THEN
        RAISE EXCEPTION
            'user limit % was already superseded at %',
            OLD.id, OLD.superseded_at
            USING ERRCODE = 'restrict_violation';
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER user_limits_append_only
    BEFORE UPDATE OR DELETE ON user_limits
    FOR EACH ROW EXECUTE FUNCTION user_limits_assert_append_only();

-- TRUNCATE bypasses row-level triggers entirely, so it needs its own
-- statement-level guard or the append-only property is one command away from
-- being false.
-- +goose StatementBegin
CREATE FUNCTION user_limits_reject_truncate() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION
        'user_limits is append-only: TRUNCATE is refused'
        USING ERRCODE = 'restrict_violation';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER user_limits_no_truncate
    BEFORE TRUNCATE ON user_limits
    FOR EACH STATEMENT EXECUTE FUNCTION user_limits_reject_truncate();

CREATE INDEX users_status_idx
    ON users (status)
    WHERE status <> 'active';

-- +goose Down

-- Reverse creation order, so every dependent object is gone before the thing it
-- depends on. Triggers are dropped implicitly with their table; the functions
-- they call are not, so they are dropped explicitly.
DROP TRIGGER IF EXISTS user_limits_set_updated_at ON user_limits;
DROP TRIGGER IF EXISTS refresh_tokens_set_updated_at ON refresh_tokens;
DROP TRIGGER IF EXISTS refresh_token_families_set_updated_at ON refresh_token_families;
DROP TRIGGER IF EXISTS user_totp_set_updated_at ON user_totp;
DROP TRIGGER IF EXISTS users_set_updated_at ON users;

DROP TRIGGER IF EXISTS user_limits_no_truncate ON user_limits;
DROP TRIGGER IF EXISTS user_limits_append_only ON user_limits;
DROP TRIGGER IF EXISTS refresh_tokens_append_only ON refresh_tokens;

DROP INDEX IF EXISTS users_status_idx;
DROP INDEX IF EXISTS user_limits_user_idx;
DROP INDEX IF EXISTS user_limits_current_key;
DROP INDEX IF EXISTS refresh_token_families_live_idx;
DROP INDEX IF EXISTS refresh_token_families_user_idx;
DROP INDEX IF EXISTS refresh_tokens_expiry_idx;
DROP INDEX IF EXISTS refresh_tokens_family_idx;
DROP INDEX IF EXISTS refresh_tokens_one_root;
DROP INDEX IF EXISTS refresh_tokens_one_successor;

DROP TABLE IF EXISTS user_limits;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS refresh_token_families;
DROP TABLE IF EXISTS user_totp;
DROP TABLE IF EXISTS users;

DROP FUNCTION IF EXISTS user_limits_reject_truncate();
DROP FUNCTION IF EXISTS user_limits_assert_append_only();
DROP FUNCTION IF EXISTS refresh_tokens_assert_append_only();

-- Namespaced `auth_`, so this drop cannot orphan catalogue_set_updated_at(),
-- betting_set_updated_at() or platform_set_updated_at() -- or the triggers in
-- 00002, 00006 and 00007 that depend on them.
DROP FUNCTION IF EXISTS auth_set_updated_at();
