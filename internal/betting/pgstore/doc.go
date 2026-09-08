// Package pgstore is the Postgres implementation of the persistence seams
// internal/betting declares.
//
// It is a separate package from internal/betting on purpose, for the reason
// internal/auth/pgstore gives about itself: the placement RULES — affordability,
// self-exclusion, responsible-gaming limits, price-move detection, round-robin
// expansion — are testable against a map, and keeping them in a package that
// does not import a database driver is what makes that true rather than
// aspirational. Everything here is SQL, row-to-domain translation, and error
// mapping. There is no policy in this package, and a review that finds one
// should move it.
//
// Three jobs, and nothing else, following the split internal/httpapi/pgstore
// established:
//
//  1. RUN THE GENERATED QUERIES. Every statement this package executes is a
//     named query in internal/platform/postgres/queries, which is what keeps the
//     whole database surface inside one `sqlc diff` drift gate and one
//     `make query-plans` index check. There is no hand-written SQL here.
//
//  2. TRANSLATE THE ROW INTO THE DOMAIN. sqlc rows carry pgtype.Float8,
//     pgtype.Timestamptz and raw enum strings, because that is what the columns
//     are. internal/betting should not know that, and a column rename should not
//     be a service change. Enum strings go through the domain's own ParseX
//     functions, each of which errors on an unrecognised value, so a schema/Go
//     divergence surfaces as a wrapped error at the read rather than as a silent
//     zero value.
//
//  3. CLASSIFY FAILURE. pgx.ErrNoRows and a handful of SQLSTATEs become
//     internal/betting's own sentinels, so the service distinguishes "already
//     placed" from "the ledger rejected this" from "the connection died" without
//     importing a driver.
//
// # Idempotency: why a primary-key collision is the mechanism, and why Redis may be down
//
// A placement carries an Idempotency-Key header, and internal/betting derives the
// wager id DETERMINISTICALLY from (userID, idempotencyKey, and for a round robin
// the combination index). The consequence is the whole design: a replayed submit
// tries to INSERT the same primary key and hits wagers_pkey, SQLSTATE 23505,
// which [Tx.InsertWager] maps to betting.ErrAlreadyPlaced. The service answers
// that by reading the EXISTING wager back and returning it — not by returning an
// error. A replayed submit is a successful submit.
//
// This is what lets CLAUDE.md §3's "Redis is never the source of truth" and
// "exactly-once placement" both be true at the same time. The Redis cache
// (betting.IdempotencyCache) is a FAST PATH ONLY: a short-TTL key that
// short-circuits before the transaction opens. When Redis is down, or cold, or
// has evicted the key, the placement proceeds, reaches Postgres, collides on the
// primary key, and returns the identical wager. Nothing about correctness
// depends on the cache having the answer, because the derived primary key was
// doing the work all along; the cache only decides whether a duplicate costs a
// round trip or a transaction.
//
// The inverse is worth stating too, because it is the mistake this shape avoids:
// an idempotency table in Postgres, written before the wager, would be a second
// record of the same fact with its own consistency question ("we wrote the key
// but the wager insert failed"). The wager row IS the record. There is nothing
// to reconcile.
//
// Not every unique violation is a replay, and this package does not treat them
// alike. legs_wager_selection_key and legs_wager_market_key mean the slip put two
// legs on one selection or one market — a malformed ticket, not a replayed one —
// and reporting either as ErrAlreadyPlaced would hand the customer somebody
// else's answer. The constraint name is inspected, never assumed, exactly as
// internal/auth/pgstore does when it separates a taken email from an id
// collision.
//
// # Why [Store.InTx] must delegate to postgres.DB.InTx
//
// migrations/00006 installs the double-entry assertion as
//
//	CREATE CONSTRAINT TRIGGER ledger_entries_balanced_at_commit
//	    AFTER INSERT OR UPDATE OR DELETE ON ledger_entries
//	    DEFERRABLE INITIALLY DEFERRED
//	    FOR EACH ROW EXECUTE FUNCTION ledger_assert_transaction_balanced();
//
// DEFERRABLE INITIALLY DEFERRED is the design and not an optimisation — a
// balanced movement is two rows and the first INSERT of any two-row movement is
// transiently unbalanced, so a NOT DEFERRABLE trigger would reject every correct
// write. The consequence is that AN UNBALANCED LEDGER WRITE RETURNS SUCCESS FROM
// EVERY INSERT AND FAILS AT COMMIT.
//
// postgres.InTx is the only helper in this tree that surfaces that: it
// propagates the commit error faithfully, rolls back on a detached context so a
// dying deadline cannot leave a backend idle in transaction, and re-raises a
// panic after closing the transaction. A hand-rolled `tx, _ := pool.Begin(...)`
// with `defer tx.Rollback(ctx)` fails all three ways, and the third failure —
// reporting a rejected money movement as written — is unrecoverable, because
// balances are derived and there is no stored value anywhere that would
// disagree.
//
// So [Store.InTx] is a delegation and nothing more. There is no BeginTx in this
// package.
//
// # Every constraint in migrations/00006 and 00008 is load-bearing here
//
// This package does not re-implement the invariants those migrations encode. It
// leans on them, which is why so little validation appears below:
//
//   - wagers_pkey is the idempotency guard. A SELECT-then-INSERT would race and
//     the loser would get a 500 where the customer should get their ticket.
//   - ledger_entries_balanced_at_commit is the ledger's central invariant, and
//     domain.NewTransaction has already refused anything unbalanced, so
//     [Tx.InsertTransaction] deliberately re-checks nothing. A third check here
//     would be a second implementation of the one rule that must have exactly
//     one.
//   - wagers_assert_transition freezes the booked terms after INSERT, so there
//     is no UPDATE statement in this package at all — grading lives in
//     internal/settlement/pgstore.
//   - wagers_refuse_excluded_user (migration 00008) refuses a ticket for a
//     self-excluded or closed customer at the database. The service reads
//     users.status first and refuses with a usable message; the trigger is what
//     makes the rule true for callers that are not the service. Both exist: a
//     database error is not a customer-facing message, and a customer-facing
//     message is not an invariant.
package pgstore
