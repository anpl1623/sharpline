/*
 * ============================================================================
 * Wager placement and the double-entry ledger write
 * ============================================================================
 *
 * (Block comment, not `--`: see the note at the top of catalogue.sql.)
 *
 * Consumer: `api` bet-slip placement and `settle` (CLAUDE.md phase 8).
 *
 * ONE TRANSACTION, NOT SEVEN STATEMENTS. Every write below is a fragment of a
 * single placement, and the schema is built so that a partial placement cannot
 * commit. The caller opens one pgx.Tx, runs the fragments through
 * gen.Queries.WithTx, and commits once. Specifically:
 *
 *   * ledger_entries carries a DEFERRABLE INITIALLY DEFERRED constraint trigger
 *     (ledger_entries_balanced_at_commit) that fires at COMMIT and rejects any
 *     transaction whose entries do not sum to exactly zero, or that has fewer
 *     than two. This is why the two halves of a movement are two separate
 *     InsertLedgerEntry calls and why that is safe: the invariant is checked when
 *     the work is finished, not after each row. Autocommitting a single entry
 *     WILL be rejected, which is correct -- one row is not a movement.
 *
 *   * wagers and legs carry deferred shape triggers (wagers_shape_at_commit,
 *     legs_shape_at_commit) that check leg-count-versus-kind and the other
 *     cross-row rules at COMMIT, so a parlay's legs may be inserted one at a time
 *     after its wager row.
 *
 *   * ordering inside the transaction is fixed by the foreign keys:
 *     round_robins -> round_robin_sizes and round_robins -> wagers -> legs, and
 *     ledger_transactions -> ledger_entries.
 *
 * NO UPDATE STATEMENTS HERE. ledger_entries and ledger_transactions refuse UPDATE
 * and DELETE by trigger outright. Grading a wager is an UPDATE to wagers/legs
 * guarded by wagers_assert_transition, and belongs to `settle`'s own query file
 * when phase 8 writes it -- placement does not grade.
 *
 * Money crosses this boundary as domain.Money (BIGINT minor units), never as a
 * float and never as a string. See sqlc.yaml's override table.
 * ============================================================================
 */


-- The customer's current balance, folded from the ledger.
--
-- Read this before accepting a slip -- it is the affordability check, and
-- CLAUDE.md section 4 is explicit that "balances are derived, never stored as a
-- mutable field". account_balances is a plain (non-materialised) view over
-- ledger_entries, so it cannot be stale; a stored balance can be, and a slip
-- validated against a stale balance is an overdraft.
--
-- The predicate is on the view's two GROUP BY keys, so Postgres pushes it below
-- the aggregate and this becomes an INDEX-ONLY SCAN of
-- ledger_entries_account_idx (account_kind, account_user_id, occurred_at)
-- INCLUDE (amount_minor, kind) -- no heap visit per entry. That is what makes
-- never materialising the balance affordable.
--
-- pgx.ErrNoRows MEANS ZERO, NOT AN ERROR, and the caller must map it. The view
-- reports one row per account that has ever been touched; an account with no
-- entries does not appear. Migration 00006 keeps that distinction on purpose --
-- "this account was touched and nets to nothing" is a different fact from "this
-- account does not exist" -- so a brand-new user with no grant yet correctly
-- returns no row.
--
-- Customer accounts only: account_user_id = $2 can never match the house and
-- issuance singletons, whose owner is NULL (NULL = NULL is NULL). An operator
-- report on those needs `account_user_id IS NULL` and is a separate query.
--
-- The view also exposes first_movement_at and last_movement_at, and this query
-- deliberately does not select them -- affordability does not need them, and sqlc
-- cannot infer a type for min()/max() inside a view definition, so selecting them
-- bare yields an `interface{}` field, which is precisely the untyped leak the
-- override table exists to prevent. A caller that genuinely wants them should cast
-- at the call site (`first_movement_at::TIMESTAMPTZ`), which gives time.Time.
--
-- name: GetAccountBalance :one
SELECT account_kind,
       account_user_id,
       balance_minor,
       entry_count
  FROM account_balances
 WHERE account_kind = @account_kind
   AND account_user_id = @account_user_id;


-- The parent row of a round robin, written before the tickets it expands into.
--
-- Must precede InsertWager for its children: wagers_round_robin_stake_fk is a
-- COMPOSITE foreign key on (round_robin_id, stake_minor) -> round_robins
-- (id, stake_per_combination_minor), which is what makes "every ticket of a round
-- robin carries the same stake" declarative instead of a trigger.
--
-- stake_per_combination_minor is the stake on EACH generated ticket, not the
-- total. RoundRobin.TotalStake() is derived and deliberately not stored.
--
-- name: InsertRoundRobin :exec
INSERT INTO round_robins (id, user_id, selection_count,
                          stake_per_combination_minor, placed_at)
VALUES (@id, @user_id, @selection_count, @stake_per_combination_minor, @placed_at);


-- One combination size a round robin expands by -- called once per entry in
-- RoundRobinParams.Sizes.
--
-- A child table rather than an array column so that distinctness (the composite
-- primary key) and "size <= selection_count" (a CHECK against the denormalised
-- copy, pinned by the composite FK) are both declarative. selection_count is that
-- denormalised copy and must equal the parent's.
--
-- name: InsertRoundRobinSize :exec
INSERT INTO round_robin_sizes (round_robin_id, selection_count, size)
VALUES (@round_robin_id, @selection_count, @size);


-- One placed ticket.
--
-- Every price-shaped value here is frozen at placement and immutable afterwards:
-- accepted_decimal is the ticket price the customer accepted -- stored, never
-- re-derived from the legs, because a correlated same-game parlay and a teaser are
-- priced by rules the leg prices do not determine -- and `rounding` is the rule the
-- ticket was written under, recorded so a later repricing (a partially-voided
-- parlay) uses it. There is no default for either; a silent default is how an
-- unintended house edge appears in a ledger.
--
-- At placement the settlement columns are NULL and the status is 'placed' or
-- 'open': wagers_return_iff_terminal makes returned_minor/net_return_minor NULL
-- exactly while the ticket is running, so passing anything else here is rejected
-- rather than accepted and later contradicted. They are still parameters because
-- `settle` re-uses this row shape and because an explicit NULL at the call site
-- reads better than a column list that changes between callers.
--
-- teaser_points is non-NULL exactly for kind='teaser' and round_robin_id exactly
-- for kind='round_robin' (both biconditional CHECKs). potential_profit_minor must
-- equal potential_payout_minor - stake_minor exactly -- it is stored rather than
-- derived so the arithmetic is asserted by the database, not trusted.
--
-- name: InsertWager :exec
INSERT INTO wagers (id, user_id, kind, status,
                    stake_minor, accepted_decimal, rounding,
                    potential_payout_minor, potential_profit_minor,
                    teaser_points, round_robin_id,
                    returned_minor, net_return_minor,
                    placed_at, transitioned_at)
VALUES (@id, @user_id, @kind, @status,
        @stake_minor, @accepted_decimal, @rounding,
        @potential_payout_minor, @potential_profit_minor,
        @teaser_points, @round_robin_id,
        @returned_minor, @net_return_minor,
        @placed_at, @transitioned_at);


-- One selection on a ticket, holding THE PRICE AT PLACEMENT TIME as values.
--
-- price_book_id / price_decimal / price_line / price_observed_at are a copied
-- domain.Price, NOT a foreign key into the prices hypertable, and that is the
-- single most important thing about this table: a booked leg must grade and pay at
-- the number the customer took, whatever the market did afterwards. Phase 1 states
-- the same rule in the domain -- a Leg holds a Price VALUE, never an id that could
-- re-resolve to a moved line.
--
-- price_line is from THIS SELECTION's own perspective (already inverted for an away
-- spread by domain.EffectiveLine), unlike markets.line which is stated from the
-- home side. teased_line is the moved line a teaser leg grades at; the untouched
-- market price stays in price_decimal/price_line so line history and CLV are not
-- corrupted by a line the book never traded.
--
-- market_type is a copy of markets.type pinned by a composite FK with ON UPDATE
-- RESTRICT, so a market's type cannot change under a booked ticket. status is
-- 'pending' at placement, with graded_at NULL (legs_graded_at_iff_graded).
--
-- name: InsertWagerLeg :exec
INSERT INTO legs (id, wager_id, event_id, market_id, market_type,
                  selection_id, role,
                  price_book_id, price_decimal, price_line, price_observed_at,
                  teased_line, status, graded_at)
VALUES (@id, @wager_id, @event_id, @market_id, @market_type,
        @selection_id, @role,
        @price_book_id, @price_decimal, @price_line, @price_observed_at,
        @teased_line, @status, @graded_at);


-- The header of one balanced money movement.
--
-- Insert this, then its entries. `kind` and `occurred_at` are repeated on every
-- entry and pinned there by a composite FK (transaction_id, kind, occurred_at), so
-- they cannot drift apart -- which is what lets a period-scoped sum over
-- ledger_entries need no join.
--
-- occurred_at is EVENT TIME, from the acting service's clock, never now(): a
-- settlement replayed from Kafka must record when the movement happened, not when
-- the row was written. created_at is the database clock and is defaulted.
--
-- wager_id is NULL for a grant, required for stake/payout/loss/refund/cash_out,
-- and optional for an operator adjustment (ledger_transactions_wager_matches_kind).
--
-- name: InsertLedgerTransaction :exec
INSERT INTO ledger_transactions (id, kind, wager_id, occurred_at)
VALUES (@id, @kind, @wager_id, @occurred_at);


-- One signed half of a money movement.
--
-- Called at least twice per transaction, and the entries of one transaction MUST
-- sum to exactly zero -- enforced by a DEFERRABLE INITIALLY DEFERRED constraint
-- trigger that fires at COMMIT. That is why this is a per-row insert rather than
-- a single multi-row statement: the invariant is transaction-scoped, so the rows
-- may arrive one at a time as long as they arrive in one transaction.
--
-- amount_minor is SIGNED -- positive credits the account, negative debits it --
-- and never zero: an entry that moves nothing is not a movement.
--
-- entry_index is the ordinal within Transaction.entries, not a surrogate key.
-- ORDER BY entry_index rehydrates Transaction.Entries() in the order the domain
-- built them, so pass the loop index, not a counter that restarts.
--
-- An account IS its (account_kind, account_user_id) pair -- there is no accounts
-- table. account_user_id is required for user_cash/user_escrow and must be NULL
-- for the house and issuance singletons (ledger_entries_owner_matches_account_kind).
--
-- kind and occurred_at must equal the parent transaction's; the composite FK
-- refuses anything else.
--
-- name: InsertLedgerEntry :exec
INSERT INTO ledger_entries (transaction_id, entry_index,
                            account_kind, account_user_id,
                            amount_minor, kind, occurred_at)
VALUES (@transaction_id, @entry_index,
        @account_kind, @account_user_id,
        @amount_minor, @kind, @occurred_at);
