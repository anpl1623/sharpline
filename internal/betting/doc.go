// Package betting turns a bet slip into a placed ticket and the balanced ledger
// movement that pays for it.
//
// It is phase 8's write path: slip validation, price-change detection,
// idempotent placement, self-exclusion, responsible-gaming limits, and cash-out
// quoting. It owns no SQL, no HTTP, and no clock — every one of those arrives
// through a port declared in ports.go, which is what makes the whole package
// unit-testable against fakes with no database in sight.
//
// What it deliberately does NOT do: grade a leg, settle a wager, or write a
// settlement transaction. Those are internal/settlement's, because they are
// driven by a results feed rather than by a customer request, and because a
// package that could both accept and settle a bet would be able to do both in
// one transaction — which is exactly the shape an operator fraud looks like.
//
// # The one invariant everything here is arranged around
//
// CLAUDE.md §4: "Legs hold the price *at placement time*, never a live
// reference." The betting service's corollary is stronger and is the reason
// this package exists as a separate layer at all:
//
//	A LEG'S PRICE IS NEVER CONSTRUCTED FROM CUSTOMER INPUT.
//
// A [SlipLeg] carries the decimal and the line the customer SAW, and those two
// numbers are used for exactly one thing: comparing against the quote re-read
// inside the placement transaction. The [domain.Price] that ends up on the
// booked leg is the store's own value, read under the same snapshot that
// inserts the wager. There is no code path in this package that builds a Price
// out of a request field, so "the customer sent us the odds they wanted" is not
// a bug that can be introduced by a careless edit — it is unrepresentable.
//
// When the re-read quote differs from the seen one, placement is REFUSED with
// [ErrPriceMoved] carrying both numbers, unless the leg carries an explicit
// [Acceptance] naming the new price. See slip.go for why an improved price is
// refused too by default, and for why a line move can never be waved through.
//
// # Idempotency: Postgres is the guard, Redis is only a shortcut
//
// A placement carries an Idempotency-Key header. The wager id is DERIVED
// DETERMINISTICALLY from (userID, idempotencyKey, combinationIndex) by
// [DeriveWagerID], a pure function over a SHA-256 of a length-prefixed
// encoding of those three values.
//
// The consequence is the whole design. A replayed submit derives the SAME
// primary key, so its INSERT collides with wagers_pkey and the store reports
// [ErrAlreadyPlaced] (SQLSTATE 23505). The placement service treats that as
// success: it reads the existing wager back and returns it with
// [Placement.Replayed] set. The customer is never double-charged, and the
// guarantee comes from a UNIQUE INDEX rather than from a lock, a lease, or a
// well-behaved client.
//
// Redis is a fast path and nothing else. [IdempotencyCache] holds a short-TTL
// key that lets a replay skip validation, quoting, the limit sums and the
// balance fold, going straight to the read-back. Every method on it is allowed
// to fail, and a nil cache disables it entirely. WHEN REDIS IS DOWN THE
// PLACEMENT PROCEEDS AND CORRECTNESS IS UNCHANGED, because Postgres was doing
// the work the whole time. That is how CLAUDE.md §3's "Redis is never the
// source of truth" and an exact-once placement live in the same system.
//
// Note what the cache deliberately does not hold: the wager itself. A cached
// ticket body would be stale the moment settlement graded it, so a replay two
// hours later would report a settled wager as still open. The cache carries
// ids; the bodies always come from Postgres.
//
// # Self-exclusion: the service is the trust boundary, the database is the
// backstop
//
// Phase 5 left open where the self-exclusion check belongs. The answer is three
// layers, and the middle one is the authoritative one:
//
//  1. THIS PACKAGE reads users.status inside the placement transaction, with
//     the row locked, and refuses 'self_excluded', 'suspended' and 'closed'.
//     This is the trust boundary: every caller that can place a bet goes
//     through [Service.Place], so there is no route around it.
//
//  2. internal/httpapi maps [ErrSelfExcluded] to 403 with a specific code. A
//     presentation concern, not a control.
//
//  3. migrations/00008 adds a BEFORE INSERT trigger on wagers that raises when
//     the user is self-excluded, so no writer at all — a repair script, a
//     future service, `make psql` — can book a bet for an excluded customer.
//
// An HTTP-middleware-only check was rejected outright, and auth.UserStatus.CanWager
// spells out why at length: a JWT is a snapshot minted at login, so a customer
// who self-excludes at 14:00 holds a token that still says "active" until it
// expires, and the minutes right after somebody decides to use the tool are the
// exact minutes it has to work. Reading the row inside the writing transaction
// is the only placement with no window.
//
// # Responsible-gaming limits are a sum over the ledger, not a counter
//
// user_limits.kind spells 'grant', 'stake' and 'loss' to match
// [domain.EntryKind] deliberately (migrations/00005 says so), which makes
// enforcement a period-scoped SUM over ledger_entries against the customer's
// cash account, evaluated inside the placement transaction under the same row
// lock as the balance. No counter is maintained, so no counter can drift from
// the rows that produced it — the same argument CLAUDE.md §4 makes about
// balances, applied to limits.
//
// 'session' is a duration, not a money sum, and is out of the placement path
// entirely: it bounds how long an authenticated session may last, which is
// internal/auth's question. limits.go states that as a biconditional and skips
// it rather than silently treating a seconds value as minor units.
//
// # Cash-out is quoted off the FAIR price minus a NAMED haircut
//
//	cashOut = round(potentialPayout × Π fairProbability(pending legs) × (1 − margin))
//
// with margin = [DefaultCashOutMarginBps] / 10000, i.e. 5%, and the rounding
// rule taken from the ticket rather than chosen fresh.
//
// The reason to quote off the FAIR (devigged, sharp-reference-book, per ADR
// 0006) price and then subtract an explicit haircut, rather than quoting off
// the offered price: it makes the book's take AUDITABLE. Quoting off the
// offered price hides the same take inside the vig, where it is entangled with
// the market's own margin and "what did the book charge me to close early"
// stops having an answer. A named constant can be reviewed, alerted on, and
// argued about; a number buried in a devig cannot.
//
// A cash-out is refused when any pending leg's reference price is older than
// [DefaultMaxFairPriceAge], when the wager is already terminal, when any leg is
// void or pushed (the ticket needs repricing, which is settlement's job under
// the ticket's own Rounding — quoting off a payout that is known to be wrong is
// the "plausible number of the right magnitude" failure wager.go warns about),
// or when the computed value is not positive.
//
// See cashout.go for the arithmetic, including a recorded disagreement with the
// formula as it was originally written in the phase brief.
//
// # Ordering inside the placement transaction, and why it is that order
//
// ONE [postgres.InTx] call. Never a hand-rolled Begin/Commit, because the
// double-entry assertion is DEFERRABLE INITIALLY DEFERRED: an unbalanced ledger
// write returns SUCCESS from every INSERT and fails at COMMIT, and InTx is the
// only helper that surfaces that error faithfully.
//
//	idempotency fast path        cache hit → read back and return
//	user status, row LOCKED      self-exclusion, and the lock that makes the
//	                             limit and balance steps race-free
//	quote re-read per leg        the price-move check; legs are built here
//	market and event state       market open, event accepting wagers
//	responsible-gaming limits    sums over ledger_entries in the window
//	balance                      the fold over ledger_entries
//	build wagers and stake txns  domain constructors do the validating
//	insert                       round robin → wagers → legs → ledger
//
// The status read takes the row lock (see [Tx.UserStatus]) and the limit and
// balance steps depend on it. Both are read-then-write over a derived quantity: two concurrent
// placements that each fold the same balance would each see enough money and
// both commit, and the ledger has no non-negative constraint to catch it —
// grant and issuance go negative by design. Serialising a single customer's
// placements on their own users row costs nothing (a customer places one bet at
// a time) and removes the overdraft entirely.
//
// NOTHING HERE IS EVER RETRIED. postgres.IsTransientConnectError gates retries
// and it covers connection failures only; a retried ledger write double-applies
// the money. A failed placement is reported, not re-attempted.
//
// # What this package refuses to do rather than guess
//
// Two ticket shapes are refused by the pricer shipped here, and both refusals
// are deliberate rather than unfinished:
//
//   - A SAME-GAME PARLAY. CLAUDE.md §4 requires a correlation adjustment for
//     legs on one event, and pricing correlated legs as independent overprices
//     the ticket in the customer's favour — a real house-edge defect, not a
//     rounding one. [IndependentPricer] refuses rather than misprice; supply a
//     [TicketPricer] built over odds.CorrelatedParlayDecimal to enable it.
//
//   - A TEASER. odds/parlay.go is explicit that a teased price cannot be
//     derived from the posted one without an empirical model of how the sport's
//     margins are distributed, and that "inventing one here would be fabricated
//     data of exactly the kind the project forbids". The ticket price of a
//     teaser is a posted ladder that this repository does not have. Supply a
//     [TicketPricer] that knows the ladder and teasers work end to end; without
//     one they are refused with [ErrTeaserUnsupported].
//
// Refusing is the right failure mode for both: a wrong ticket price is written
// into a wagers row that the schema then makes immutable, so it is wrong
// forever, and it is wrong in the direction nobody audits.
package betting
