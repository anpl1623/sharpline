-- =============================================================================
-- 00008  the self-exclusion backstop
-- =============================================================================
--
-- CLAUDE.md §6 (Account): "responsible-gaming-style self-imposed limits (a nod
-- to how the real domain works)."
--
-- One trigger. No tables, no columns, no indexes. It refuses an INSERT into
-- `wagers` whose user is self-excluded or closed.
--
-- WHY A THIRD LAYER, WHEN TWO ALREADY EXIST
-- -----------------------------------------
-- Self-exclusion is enforced in three places, and each one is answering a
-- different question:
--
--   1. internal/betting's placement service reads users.status INSIDE the
--      transaction that inserts the wager and refuses on 'self_excluded'. This
--      is the AUTHORITATIVE check and the only one that produces a usable
--      error message. internal/auth/status.go argues at length for that
--      placement -- a JWT claim is a snapshot minted at login, so an HTTP
--      middleware check has a window exactly the width of the access-token
--      lifetime, and "the ONE moment a responsible-gaming control matters most
--      is the minutes right after somebody decides to use it".
--
--   2. The HTTP layer maps that sentinel to 403 with a specific error code, so
--      the customer is told what happened rather than shown a 500.
--
--   3. This trigger. It answers the question the other two cannot: "what stops
--      a caller that is not the placement service?" A `make psql` session, an
--      admin console INSERT, a future service written by somebody who did not
--      read internal/betting, a repair script -- none of them go through layer
--      1, and all of them can reach this table. 00005 already made the
--      corresponding argument for `refresh_tokens_append_only`: the database is
--      what makes an invariant true for every writer rather than for the
--      writers we remembered.
--
-- The three are NOT redundant. Layer 1 exists because a database error is not
-- a customer-facing message; layer 3 exists because a customer-facing message
-- is not an invariant. Deleting either one is a downgrade.
--
-- WHICH STATUSES THIS REFUSES, AND WHICH IT DELIBERATELY DOES NOT
-- ---------------------------------------------------------------
-- users.status is active | suspended | self_excluded | closed
-- (users_status_defined, 00005). auth.UserStatus.CanWager() returns true for
-- 'active' ONLY, so the service refuses the other three. This trigger refuses
-- exactly TWO of them, and the difference is deliberate:
--
--   self_excluded   REFUSED. This is the customer's own protection, and it is
--                   the one control that must not be defeatable by an operator
--                   with a psql prompt. That asymmetry is the entire point of
--                   putting it in the database: an operator can lift a
--                   suspension they imposed, but nobody may quietly book a bet
--                   for a customer who asked the system to stop them.
--
--   closed          REFUSED. A closed account is terminated. CanAuthenticate()
--                   is false for it (00005 / auth/status.go), so no session can
--                   exist to place a bet, and there is no other legitimate
--                   writer. A wager appearing on a closed account is a bug in
--                   whatever wrote it, every time, so refusing it loses nothing
--                   and catches a real class of mistake.
--
--   suspended       NOT REFUSED, and this is the one that needs an argument.
--                   Suspension is an OPERATOR action, reversible, and imposed
--                   while an account is under review. CLAUDE.md §6 gives the
--                   admin console "manual settlement" duties, and a correction
--                   made during a review -- re-booking a ticket that was voided
--                   in error, for instance -- has to be able to write a wager
--                   row for the account under review. Refusing it here would
--                   force an operator to LIFT THE SUSPENSION in order to
--                   perform the correction, which re-opens the account to the
--                   customer for the duration of the fix. That is a strictly
--                   worse control than the one it replaces.
--
--                   The customer-facing route is still closed: CanWager() is
--                   false for 'suspended', so internal/betting refuses it at
--                   layer 1 and the API returns 403. What is left open is the
--                   operator route, which is audited (audit_log, 00007) and is
--                   the only caller that legitimately has it.
--
-- So the rule this migration encodes is narrower than the service's, on
-- purpose: the DATABASE refuses the states with NO legitimate writer at all;
-- the SERVICE refuses everything that is not 'active'.
--
-- INSERT ONLY. NOT UPDATE.
-- ------------------------
-- A self-excluded customer's ALREADY-PLACED wagers must still grade, settle,
-- and pay out. Their money is in escrow (AccountKindUserEscrow) and the only
-- way it comes back is `settle` running the normal UPDATE path. A trigger that
-- also fired on UPDATE would strand a self-excluded customer's open positions
-- forever -- turning the tool that exists to protect them into a way to lose
-- their balance, which is exactly the failure mode auth/status.go refuses when
-- it keeps LOGIN permitted for a self-excluded account.
--
-- user_id itself is already immutable: wagers_assert_transition (00006) lists
-- it among the booked terms and refuses any UPDATE that changes it. So there is
-- no route by which an existing wager can be moved onto an excluded account
-- either, and this trigger does not need to cover one.
--
-- WHY NO `FOR UPDATE` ON THE users ROW
-- ------------------------------------
-- The SELECT below is lock-free, matching the GetUserStatus query in
-- queries/betting.sql, and for the same two reasons.
--
-- It would not close the race it appears to. Under READ COMMITTED a BEFORE
-- trigger reads the INSERT statement's snapshot, so an exclusion committed
-- microseconds later is invisible either way; a row lock would only serialise
-- the two, and auth/status.go already accepts BOTH orderings as correct:
-- "an exclusion that commits first is seen, and one that commits after was not
-- in force when the wager was accepted. Either outcome is defensible to the
-- customer, which is the actual requirement."
--
-- And it would cost something real. FOR UPDATE on `users` would serialise every
-- placement by one customer against their own profile row, and would take a
-- lock that the auth service's own writes (password change, status change,
-- updated_at stamp) contend for -- introducing a deadlock cycle between
-- placement and account management to buy a guarantee nobody asked for.
--
-- Note that the row is already lightly locked regardless: the foreign key
-- wagers.user_id -> users(id) makes PostgreSQL take FOR KEY SHARE on it during
-- the INSERT. That blocks a DELETE or a change to `id`, which is exactly the
-- guarantee the FK needs and no more.
--
-- TRIGGER FIRING ORDER
-- --------------------
-- PostgreSQL fires BEFORE row triggers in alphabetical order by trigger name.
-- On INSERT INTO wagers this is the only BEFORE row trigger there is --
-- wagers_set_updated_at is BEFORE UPDATE and wagers_assert_transition is
-- BEFORE UPDATE OR DELETE -- so ordering is not load-bearing today. It is
-- recorded anyway, because a future BEFORE INSERT trigger sorting before
-- `wagers_refuse_excluded_user` would run first, and a trigger that runs before
-- the exclusion check must not have side effects.
--
-- COST
-- ----
-- One primary-key lookup on `users` per inserted wager row. A round robin
-- inserts one row per combination, so a 4-selection by-2s ticket pays for six.
-- Measured against booking a bet for somebody who asked not to be able to place
-- one, that is not a cost worth optimising.
--
-- DEPENDENCIES
-- ------------
--   users   (id TEXT PRIMARY KEY, status TEXT NOT NULL)  00005
--   wagers  (user_id TEXT NOT NULL)                      00006
--
-- Both are asserted by the preflight below, together with the `status` column
-- itself -- the trigger body reads a column by name, and a bare
-- `record has no field` failure at INSERT time, months later, is a far worse
-- diagnostic than a refused migration.
--
-- +goose Up

-- ---------------------------------------------------------------------------
-- Preflight. CLAUDE.md §12's "fail fast and loudly on a bad config", applied to
-- schema prerequisites. Mirrors the pattern 00006 and 00007 established.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
DO $preflight$
DECLARE
    missing TEXT[] := ARRAY[]::TEXT[];
BEGIN
    IF to_regclass('public.users') IS NULL THEN
        missing := missing || 'users (00005_accounts_and_auth.sql)';
    END IF;
    IF to_regclass('public.wagers') IS NULL THEN
        missing := missing || 'wagers (00006_wagers_and_ledger.sql)';
    END IF;

    IF array_length(missing, 1) IS NOT NULL THEN
        RAISE EXCEPTION
            'migration 00008_self_exclusion_guard requires table(s) that do not exist: %',
            array_to_string(missing, ', ')
            USING HINT = 'This migration installs a trigger on wagers that reads users.status. '
                         'Both migrations must be numbered lower than 00008.';
    END IF;

    -- The trigger body reads users.status by name. Assert the column rather
    -- than discovering its absence at the first INSERT.
    IF NOT EXISTS (
        SELECT 1
          FROM pg_attribute a
          JOIN pg_class     t ON t.oid = a.attrelid
         WHERE t.relname = 'users'
           AND a.attname = 'status'
           AND a.attnum > 0
           AND NOT a.attisdropped
    ) THEN
        RAISE EXCEPTION
            'migration 00008_self_exclusion_guard requires users.status'
            USING HINT = '00005_accounts_and_auth.sql declares it as TEXT NOT NULL DEFAULT ''active'' '
                         'constrained by users_status_defined to active | suspended | self_excluded | closed.';
    END IF;
END
$preflight$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- The guard itself.
--
-- Namespaced `betting_` for the reason 00006 and 00007 both spell out: a bare
-- name is the one every concurrently-authored migration reaches for, two of
-- them collide on the way up, and the first Down to run drops a function the
-- other's triggers still need. 00006 owns betting_set_updated_at() and
-- betting_reject_truncate(); this file owns exactly one function and drops
-- exactly that one.
--
-- ERRCODE 'restrict_violation' (23001) matches every other refusal on this
-- table -- wagers_assert_transition and legs_assert_transition both raise it --
-- so a caller distinguishing "the schema refused this" from "the ledger did
-- not balance" (23514) or "the connection died" reads one SQLSTATE family
-- rather than parsing a message.
--
-- The message names the user id and the status, and the HINT names the layer
-- that should have caught it first. A human who sees this in a log has been
-- handed a bug in a caller, not a customer-facing event: the customer-facing
-- path returns 403 long before reaching here.
-- ---------------------------------------------------------------------------
-- +goose StatementBegin
CREATE FUNCTION betting_refuse_excluded_user() RETURNS trigger
    LANGUAGE plpgsql
AS $betting_refuse_excluded_user$
DECLARE
    user_status TEXT;
BEGIN
    -- Lock-free, deliberately. See the header.
    --
    -- No `IF NOT FOUND` branch: wagers.user_id is NOT NULL and carries a
    -- foreign key to users(id), so the row is guaranteed to exist by the time
    -- any BEFORE trigger runs on a row that will actually be stored. If the FK
    -- were ever dropped, user_status stays NULL, the comparison below is NULL,
    -- and the INSERT is ALLOWED -- which is the correct failure direction for a
    -- backstop whose prerequisite has been removed, because the alternative is
    -- refusing every wager in the system on a schema change nobody connected to
    -- this file.
    SELECT u.status INTO user_status
      FROM users u
     WHERE u.id = NEW.user_id;

    IF user_status = 'self_excluded' THEN
        RAISE EXCEPTION
            'user % is self-excluded and cannot be booked a wager (attempted wager %)',
            NEW.user_id, NEW.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'Self-exclusion is the customer''s own responsible-gaming control. '
                            'internal/betting reads users.status inside the placement transaction '
                            'and refuses first; reaching this trigger means a caller bypassed it. '
                            'Settlement of their EXISTING wagers is unaffected -- this fires on '
                            'INSERT only, so open positions still grade and pay out.';
    END IF;

    IF user_status = 'closed' THEN
        RAISE EXCEPTION
            'user % is closed and cannot be booked a wager (attempted wager %)',
            NEW.user_id, NEW.id
            USING ERRCODE = 'restrict_violation',
                  HINT    = 'A closed account cannot authenticate, so no session can have placed '
                            'this. Settlement of their existing wagers is unaffected.';
    END IF;

    -- 'suspended' falls through on purpose. See the header: the customer route
    -- is closed at the service, and the operator route must stay open so a
    -- correction during a review does not require lifting the suspension.
    RETURN NEW;
END;
$betting_refuse_excluded_user$;
-- +goose StatementEnd

COMMENT ON FUNCTION betting_refuse_excluded_user() IS
    'BEFORE INSERT trigger on wagers: refuses a ticket for a self_excluded or closed user. SQLSTATE 23001. Deliberately silent on ''suspended'' (the operator correction path) and deliberately not installed on UPDATE (an excluded customer''s open positions must still settle).';

CREATE TRIGGER wagers_refuse_excluded_user
    BEFORE INSERT ON wagers
    FOR EACH ROW EXECUTE FUNCTION betting_refuse_excluded_user();

-- +goose Down

-- Reversibility, per CLAUDE.md §12. The exact inverse of the Up, in the only
-- order that works: the trigger references the function, so dropping the
-- function first fails with a dependency error.
--
-- Unlike 00006's Down, this one destroys no data. Rolling it back removes a
-- guard, which is a security regression rather than a data-loss event -- but
-- note that it leaves layer 1 (internal/betting) and layer 2 (the 403) intact,
-- so a customer who has self-excluded is still refused through every route the
-- application offers. What is lost is the guarantee for callers that are not
-- the application.

DROP TRIGGER IF EXISTS wagers_refuse_excluded_user ON wagers;

DROP FUNCTION IF EXISTS betting_refuse_excluded_user();
