// The placement service: one slip in, one transaction, one or many tickets and
// the balanced ledger movements that pay for them.
//
// Read doc.go first. It carries the argument for deriving the wager id, for
// where the self-exclusion check has to live, for why the users row is locked,
// and for the two ticket shapes this package refuses to price. This file is the
// code those arguments describe.
package betting

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

// Defaults. Each is overridable through [Options]; the zero value means "use
// this".
const (
	// DefaultMaxQuoteAge is how old a market quote may be and still be bet
	// against.
	//
	// # This number is derived from the ingest cadence, and must stay that way
	//
	// It used to read 30 * time.Second, justified by the claim that
	// "internal/ingest polls a live market far more often than that". That claim
	// was false against the scheduler this repository actually ships:
	// scheduler.DefaultTiers sets the live tier to a FIXED 90-second interval,
	// and that 90 is not free to move — ADR 0003 derives it from the provider's
	// monthly credit budget, so shortening it changes the bill rather than the
	// staleness.
	//
	// The consequence was measured against a running stack rather than reasoned
	// about: sampling every open market on every live event, the freshest
	// available quote had a median age of 52s, p90 91s, p99 130s and a maximum
	// of 150s, and was older than the 30s limit 76% of the time. In-play
	// betting — which the event-status check at [Service.priceSlip] goes out of
	// its way to permit, and which CLAUDE.md §6 lists under Betting — was
	// therefore refused with `market_unavailable` roughly three times in four.
	// The horizon was not measuring staleness; it was rejecting the system's own
	// freshest data.
	//
	// Two effects set the floor, and only the first is a poll interval:
	//
	//   - The live tier refreshes a market at most every 90s, so a quote is
	//     between 0 and 90s old in the healthy steady state.
	//   - Change detection (CLAUDE.md §5: "Hash each normalized market to
	//     suppress no-op updates") means a market whose line did not move
	//     produces no record at all, so its newest row survives a sweep and its
	//     age runs past one interval. That is the 150s tail above, and it is the
	//     generator behaving correctly, not the poller falling behind.
	//
	// Three minutes is two live intervals, which covers one fully suppressed
	// sweep with margin over the measured p99. It is still a real check and
	// still fails loudly on the condition the original was written for: a quote
	// this old means two consecutive live sweeps produced nothing, which is a
	// wedged poller or a provider that has stopped answering, not a quiet line.
	//
	// KNOWN LIMIT, recorded rather than papered over. Because of change
	// detection, `observed_at` is when the line last MOVED, not when the book
	// last CONFIRMED it, so no fixed horizon separates "nobody is watching this
	// market" from "this market is quiet". The durable fix is for the ingest
	// writer to stamp a last-confirmed instant even when it suppresses the
	// publish, and for this check to read that instead. Until it exists, this
	// constant is a bound on the wrong clock — a deliberately generous one, so
	// that it errs towards laying a quiet line rather than towards refusing
	// every in-play bet.
	//
	// Deliberately looser than [DefaultMaxFairPriceAge] (10s). A placement is a
	// customer taking a number the book posted; a cash-out is the book quoting
	// a number back. The side making the offer wears the staleness risk.
	DefaultMaxQuoteAge = 3 * time.Minute

	// DefaultIdempotencyTTL is how long the Redis fast path remembers a key.
	//
	// It bounds a SHORTCUT, not the guarantee: expiring it early costs a
	// replayed submit some extra reads and changes no outcome, because
	// wagers_pkey is doing the actual work. Ten minutes comfortably covers the
	// retry window of any client and of any proxy in front of one, and keeping
	// it short keeps the key space bounded without a sweeper.
	DefaultIdempotencyTTL = 10 * time.Minute
)

// Options configures a [Service]. Every field is optional; zero means the
// documented default.
//
// The three ports that have no default — [Store], [TicketPricer] and a clock —
// are constructor arguments rather than option fields, so a Service cannot be
// built without them and there is no half-configured state to check for at the
// first placement. internal/pricing draws the same line for the same reason:
// "Configuration fails at construction, loudly, rather than at the first
// record."
type Options struct {
	// Cache is the Redis idempotency fast path. Nil disables it, which is what
	// a unit test wants and what a cold start does; correctness is unchanged
	// either way (doc.go).
	Cache IdempotencyCache

	// Wagers and FairPrices are required by [Service.CashOutQuote] and by
	// nothing else. Leaving them nil is legal and makes cash-out quoting return
	// [ErrInvalidOptions] — an `api` binary that does not expose the endpoint
	// should not have to construct the ports behind it.
	Wagers     Wagers
	FairPrices FairPrices

	// MaxQuoteAge is the placement staleness horizon. Zero means
	// [DefaultMaxQuoteAge]. Negative is refused rather than clamped: a negative
	// horizon makes every quote stale and would refuse every bet, which is a
	// configuration error that must fail at startup rather than at 3am.
	MaxQuoteAge time.Duration

	// MaxFairPriceAge is the cash-out staleness horizon. Zero means
	// [DefaultMaxFairPriceAge].
	MaxFairPriceAge time.Duration

	// CashOutMarginBps is the book's take on an early close, in basis points.
	// Zero means [DefaultCashOutMarginBps]; to mean an actual zero margin, set
	// [Options.ZeroCashOutMargin] as well.
	CashOutMarginBps int

	// ZeroCashOutMargin makes a zero CashOutMarginBps mean an actual zero
	// margin rather than the default.
	//
	// It exists because "zero means the default" and "zero is a legal value"
	// cannot both be true of one int field, and the alternatives are worse: a
	// *int makes every call site allocate, and a sentinel like -1 hides a
	// promotional setting behind a number that looks like a bug. An explicit
	// bool says what it means at the call site.
	ZeroCashOutMargin bool

	// IdempotencyTTL is how long the fast path remembers a key. Zero means
	// [DefaultIdempotencyTTL].
	IdempotencyTTL time.Duration

	// Logger receives the events that are worth a line and are not errors the
	// caller already has: a cache failure that was absorbed, a replay that was
	// served. Nil means slog.Default().
	Logger *slog.Logger
}

// Service places wagers.
//
// It holds no mutable state — every field is set at construction and read
// afterwards — so one instance is shared by every request goroutine, which is
// what CLAUDE.md §12's "no global mutable state, dependencies are
// constructor-injected" produces when applied to a request-scoped service.
type Service struct {
	store  Store
	pricer TicketPricer
	now    Clock

	cache      IdempotencyCache
	wagers     Wagers
	fairPrices FairPrices

	maxQuoteAge      time.Duration
	maxFairPriceAge  time.Duration
	cashOutMarginBps int
	idempotencyTTL   time.Duration

	log *slog.Logger
}

// NewService builds a placement service.
//
// store, pricer and clock are required. The clock is a parameter rather than a
// package-level time.Now so that ONE placement has ONE instant — placed_at, the
// limit windows, the staleness horizon and the ledger's occurred_at are all the
// same value, read once at the top of [Service.Place]. Three calls to time.Now()
// in one placement put three instants in one ticket.
func NewService(store Store, pricer TicketPricer, clock Clock, opts Options) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalidOptions)
	}
	if pricer == nil {
		return nil, fmt.Errorf("%w: nil ticket pricer", ErrInvalidOptions)
	}
	if clock == nil {
		return nil, fmt.Errorf("%w: nil clock", ErrInvalidOptions)
	}
	if opts.MaxQuoteAge < 0 {
		return nil, fmt.Errorf("%w: max quote age %s is negative", ErrInvalidOptions, opts.MaxQuoteAge)
	}
	if opts.MaxFairPriceAge < 0 {
		return nil, fmt.Errorf("%w: max fair price age %s is negative", ErrInvalidOptions, opts.MaxFairPriceAge)
	}
	if opts.IdempotencyTTL < 0 {
		return nil, fmt.Errorf("%w: idempotency ttl %s is negative", ErrInvalidOptions, opts.IdempotencyTTL)
	}
	if opts.CashOutMarginBps < 0 || opts.CashOutMarginBps > MaxCashOutMarginBps {
		return nil, fmt.Errorf("%w: cash out margin %d bps is outside [0, %d]",
			ErrInvalidOptions, opts.CashOutMarginBps, MaxCashOutMarginBps)
	}

	s := &Service{
		store:            store,
		pricer:           pricer,
		now:              clock,
		cache:            opts.Cache,
		wagers:           opts.Wagers,
		fairPrices:       opts.FairPrices,
		maxQuoteAge:      orDuration(opts.MaxQuoteAge, DefaultMaxQuoteAge),
		maxFairPriceAge:  orDuration(opts.MaxFairPriceAge, DefaultMaxFairPriceAge),
		cashOutMarginBps: opts.CashOutMarginBps,
		idempotencyTTL:   orDuration(opts.IdempotencyTTL, DefaultIdempotencyTTL),
		log:              opts.Logger,
	}
	if s.cashOutMarginBps == 0 && !opts.ZeroCashOutMargin {
		s.cashOutMarginBps = DefaultCashOutMarginBps
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	return s, nil
}

func orDuration(v, fallback time.Duration) time.Duration {
	if v == 0 {
		return fallback
	}
	return v
}

// PlaceRequest is one submitted placement.
type PlaceRequest struct {
	UserID domain.UserID

	// IdempotencyKey is the client's declaration that two submits are one
	// request. It is REQUIRED — see [ErrIdempotencyKeyRequired] for why an
	// optional key gives the money path at-least-once semantics.
	IdempotencyKey string

	Slip Slip

	// Audit is the provenance stamped onto the audit row written for every
	// ticket this request books, in the same transaction as the ticket. The
	// zero value is legal and means "no HTTP request produced this" — the row
	// is still written, with its correlation columns null.
	//
	// It travels on the request for the same reason httpapi.SetLimit carries
	// one: the request id, the client address and the trace ids are facts the
	// TRANSPORT knows and this package cannot discover. It carries no instant;
	// see [AuditContext] for why the placement's own clock supplies that.
	Audit AuditContext
}

// Placement is the result of a successful placement.
type Placement struct {
	// Wagers are the tickets that were booked, in combination order for a round
	// robin. Exactly one for every other kind.
	Wagers []domain.Wager

	// RoundRobin is the parent expansion, zero unless the placement was one.
	// Use RoundRobin.IsZero() to distinguish; the domain uses value types with
	// IsZero throughout rather than pointers, and a pointer here would be the
	// only nilable field in the package.
	RoundRobin domain.RoundRobin

	// Replayed reports that this placement had already happened and the tickets
	// were read back rather than written. It is NOT an error condition — see
	// [ErrAlreadyPlaced] — and a caller should render it exactly as a first
	// placement, optionally logging the replay.
	Replayed bool
}

// Place validates a slip and books it, idempotently.
//
// The ordering inside the transaction, and the reason for it, is in doc.go. The
// short version: the users row is locked first, and everything read-then-written
// afterwards depends on that lock.
//
// ON FAILURE NOTHING IS RETRIED. postgres.IsTransientConnectError gates retries
// and covers connection failures only; a retried ledger write double-applies the
// money. A caller that wants a retry re-submits with the SAME idempotency key,
// which is safe by construction and is the entire point of deriving the id.
func (s *Service) Place(ctx context.Context, req PlaceRequest) (Placement, error) {
	if req.UserID.IsZero() {
		return Placement{}, fmt.Errorf("betting: place: %w", domain.ErrEmptyID)
	}
	key, err := normaliseIdempotencyKey(req.IdempotencyKey)
	if err != nil {
		return Placement{}, err
	}
	if err := req.Slip.Validate(); err != nil {
		return Placement{}, err
	}

	// ONE instant for the whole placement. See [NewService].
	now := s.now().UTC()

	ids, err := s.deriveIDs(req.UserID, key, req.Slip)
	if err != nil {
		return Placement{}, err
	}

	if placement, hit := s.tryFastPath(ctx, req.UserID, key, ids); hit {
		return placement, nil
	}

	var placement Placement
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		var txErr error
		placement, txErr = s.place(ctx, tx, req, ids, now)
		return txErr
	})
	if err != nil {
		return Placement{}, err
	}

	s.recordFastPath(ctx, req.UserID, key, placement)
	return placement, nil
}

// derivedIDs is every identifier this placement will write, computed before the
// transaction opens.
//
// They are derived up front rather than as they are needed because the fast
// path needs the wager ids without doing any of the work, and because deriving
// them is the only step that can fail for a reason the customer can fix (an
// over-long idempotency key) — better to find that out before a connection is
// checked out and a row is locked.
type derivedIDs struct {
	roundRobin domain.RoundRobinID
	wagers     []domain.WagerID
}

func (s *Service) deriveIDs(user domain.UserID, key string, slip Slip) (derivedIDs, error) {
	count, err := slip.TicketCount()
	if err != nil {
		return derivedIDs{}, err
	}

	ids := derivedIDs{wagers: make([]domain.WagerID, count)}
	for i := range count {
		// The combination ordinal is the ticket's index for a round robin and
		// [noCombination] for everything else, and for a single ticket those
		// are the same value — see the constant's comment for why that
		// collision is the correct behaviour rather than a bug.
		id, err := DeriveWagerID(user, key, i)
		if err != nil {
			return derivedIDs{}, err
		}
		ids.wagers[i] = id
	}

	if slip.Kind == domain.WagerKindRoundRobin {
		parent, err := DeriveRoundRobinID(user, key)
		if err != nil {
			return derivedIDs{}, err
		}
		ids.roundRobin = parent
	}
	return ids, nil
}

// tryFastPath answers a replay from the Redis-cached key without validating,
// quoting, summing limits or folding the balance.
//
// It still reads the wagers back from Postgres, because the cache holds ids and
// not bodies (see [IdempotencyCache]) — a cached ticket would be stale the
// moment settlement graded it.
//
// EVERY FAILURE HERE IS ABSORBED. A cache error, a cache miss, a set of ids
// that does not match what was derived, a read-back that comes up short: all of
// them fall through to the ordinary path, which reaches the same answer through
// wagers_pkey. That is what "Redis is never the source of truth" means when it
// is written as code rather than as a comment.
func (s *Service) tryFastPath(ctx context.Context, user domain.UserID, key string, ids derivedIDs) (Placement, bool) {
	if s.cache == nil {
		return Placement{}, false
	}

	cached, found, err := s.cache.Lookup(ctx, user, key)
	if err != nil {
		s.log.Warn("idempotency cache lookup failed; falling through to postgres",
			slog.String("user_id", user.String()),
			slog.String("error", err.Error()),
		)
		return Placement{}, false
	}
	if !found || len(cached) != len(ids.wagers) {
		return Placement{}, false
	}

	var placement Placement
	err = s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		read, err := s.readBack(ctx, tx, user, ids.wagers)
		if err != nil {
			return err
		}
		placement = Placement{Wagers: read, Replayed: true}
		return nil
	})
	if err != nil {
		s.log.Warn("idempotency fast path could not read the cached wagers back; re-placing",
			slog.String("user_id", user.String()),
			slog.String("error", err.Error()),
		)
		return Placement{}, false
	}
	return placement, true
}

// recordFastPath notes the placed ids in the cache. Best effort by definition:
// the return value is logged and dropped, because a cache write failing has no
// effect on a placement that has already committed.
func (s *Service) recordFastPath(ctx context.Context, user domain.UserID, key string, placement Placement) {
	if s.cache == nil || len(placement.Wagers) == 0 {
		return
	}
	ids := make([]domain.WagerID, len(placement.Wagers))
	for i, w := range placement.Wagers {
		ids[i] = w.ID()
	}
	if err := s.cache.Record(ctx, user, key, ids, s.idempotencyTTL); err != nil {
		s.log.Warn("idempotency cache write failed; the placement is unaffected",
			slog.String("user_id", user.String()),
			slog.String("error", err.Error()),
		)
	}
}

// place is the body of the placement transaction. See doc.go for the ordering.
func (s *Service) place(
	ctx context.Context,
	tx Tx,
	req PlaceRequest,
	ids derivedIDs,
	now time.Time,
) (Placement, error) {
	// 1. Status, WITH THE USERS ROW LOCKED. Everything after this depends on
	//    the lock; see [Tx.UserStatus].
	if err := s.checkStatus(ctx, tx, req.UserID); err != nil {
		return Placement{}, err
	}

	// 2. HAS THIS SUBMIT ALREADY BOOKED? If so, answer with the tickets it
	//    wrote and do no market work at all.
	//
	//    This check must come BEFORE step 3, and the ordering is the whole
	//    point of it. A replay is otherwise detected by the INSERT colliding
	//    with its derived primary key — which is a sound detector, but it is
	//    reached only after the quotes have been re-read and the price-move
	//    rule applied. A client that retried after a timeout is retrying
	//    SECONDS OR MINUTES LATER, by which time the quote it sent is very
	//    likely stale or moved, so the refusal fires first and the customer is
	//    told 409 for a bet they already hold. That is the exact scenario an
	//    idempotency key exists to answer, and answering it with an error means
	//    the client still cannot tell whether its first attempt landed.
	//
	//    It is a primary-key lookup, and it is taken UNDER THE LOCK from step 1,
	//    so it is not the "lookup that could race" the derived id was chosen to
	//    avoid: one customer's placements are serialised by that lock. The
	//    INSERT collision remains the backstop and is unchanged — if this lookup
	//    ever missed, writeTicket would still refuse to write a second ticket.
	//    Nothing here weakens the guarantee; it only stops a market condition
	//    from masking it.
	if replay, found, err := s.replayed(ctx, tx, req, ids); err != nil {
		return Placement{}, err
	} else if found {
		return replay, nil
	}

	// 3. Re-read every quote and build the legs from the store's own Price
	//    values. This is where the price-move check happens, and it is the only
	//    route by which a price reaches a leg.
	booked, err := s.bookLegs(ctx, tx, req.Slip, ids.wagers[0], now)
	if err != nil {
		return Placement{}, err
	}

	// 4. Affordability, against the total the slip actually risks.
	total, err := req.Slip.TotalStake()
	if err != nil {
		return Placement{}, err
	}
	if err := evaluateLimits(ctx, tx, req.UserID, total, now); err != nil {
		return Placement{}, err
	}
	if err := s.checkBalance(ctx, tx, req.UserID, total); err != nil {
		return Placement{}, err
	}

	// 5. Build and write. The domain constructors do the validating, and every
	//    refusal they produce is a bug here rather than a customer error —
	//    which is why they are wrapped rather than mapped to a sentinel.
	if req.Slip.Kind == domain.WagerKindRoundRobin {
		return s.placeRoundRobin(ctx, tx, req, ids, booked, now)
	}
	return s.placeSingle(ctx, tx, req, ids, booked, now)
}

// replayed reports whether this submit's single derived ticket already exists,
// and returns it if it does.
//
// # Why a missing ticket is not an error
//
// The overwhelmingly common case is a first submit, where the lookup finds
// nothing. [ErrWagerNotFound] is the expected answer, not a failure, and it is
// the only error swallowed here: anything else — a dead connection above all —
// is returned, because treating an outage as "not placed yet" would send the
// placement on to write a ticket that may already exist.
//
// # A ROUND ROBIN IS DELIBERATELY NOT SHORT-CIRCUITED HERE, and that is a known
// # limitation rather than an oversight
//
// A round robin's [Placement] carries the parent expansion alongside the
// tickets, and the parent is NOT directly recoverable from stored rows: the
// round_robins table holds the selection count, the per-combination stake and
// the sizes, but not the parent's leg set — that exists only as the union of
// the tickets' legs. Rebuilding it here would mean either unioning the
// read-back tickets' legs and re-deriving the sizes from their arities, or
// rebuilding it from THIS REQUEST's slip. The second is wrong outright, because
// a replay must report what the original submit booked and not what this one
// asked for; the first is defensible but is enough reconstruction logic to want
// its own tests and its own review.
//
// So a round robin keeps the original detector: the per-ticket INSERT collides
// and [Service.writeTicket] reads back. The consequence is real and worth
// stating — a round robin retried after its quotes have moved is still refused
// with a price or market error rather than answered with its existing tickets.
// The MONEY is safe either way, because the derived primary keys still make a
// second booking impossible; only the retry's answer is worse. Everything else
// (straight, parlay, teaser) is short-circuited above.
func (s *Service) replayed(
	ctx context.Context,
	tx Tx,
	req PlaceRequest,
	ids derivedIDs,
) (Placement, bool, error) {
	if req.Slip.Kind == domain.WagerKindRoundRobin {
		return Placement{}, false, nil
	}

	first, err := s.readBackOne(ctx, tx, req.UserID, ids.wagers[0])
	switch {
	case errors.Is(err, ErrWagerNotFound):
		return Placement{}, false, nil
	case err != nil:
		return Placement{}, false, err
	}

	return Placement{Wagers: []domain.Wager{first}, Replayed: true}, true, nil
}

// checkStatus reads users.status under the row lock and refuses anything but
// 'active'.
//
// THE UNRECOGNISED CASE FAILS CLOSED. auth.ParseUserStatus returns an error for
// a value this build does not know, and the answer to that is to refuse the
// bet: a status column whose CHECK constraint has grown a value the binary
// predates is exactly the situation where "assume it is fine" is wrong, and
// auth.UserStatus has an invalid zero value for the same reason ("a status that
// was never set must not silently read as 'active', because 'active' is the one
// value that permits everything").
func (s *Service) checkStatus(ctx context.Context, tx Tx, user domain.UserID) error {
	raw, err := tx.UserStatus(ctx, user)
	if err != nil {
		return fmt.Errorf("betting: read status for %s: %w", user, err)
	}
	status, err := auth.ParseUserStatus(raw)
	if err != nil {
		return fmt.Errorf("betting: user %s has an unrecognised status: %w: %w",
			user, err, ErrAccountNotWagerable)
	}
	if status.CanWager() {
		return nil
	}
	if status == auth.UserStatusSelfExcluded {
		// Its own sentinel, because the response it earns is different in kind
		// from a suspension. See [ErrSelfExcluded].
		return fmt.Errorf("betting: user %s: %w", user, ErrSelfExcluded)
	}
	return fmt.Errorf("betting: user %s is %s: %w", user, status, ErrAccountNotWagerable)
}

// checkBalance folds the customer's cash balance and refuses an unfunded slip.
//
// The fold is taken under the lock from [Service.checkStatus], so it cannot be
// raced by a concurrent placement of the same customer's. It is a fold and not
// a stored figure because CLAUDE.md §4 says balances are derived — and
// migrations/00006 spells out the consequence: "a bet slip validated against a
// stale balance is an overdraft."
func (s *Service) checkBalance(ctx context.Context, tx Tx, user domain.UserID, total domain.Money) error {
	cash, err := domain.UserCashAccount(user)
	if err != nil {
		return fmt.Errorf("betting: cash account for %s: %w", user, err)
	}
	balance, err := tx.Balance(ctx, cash)
	if err != nil {
		return fmt.Errorf("betting: read balance for %s: %w", user, err)
	}
	if balance.Compare(total) < 0 {
		return &ShortFall{Available: balance.MinorUnits(), Required: total.MinorUnits()}
	}
	return nil
}

// bookLegs re-reads every leg's quote inside the transaction, applies the
// price-move rule, checks the market and the event, and builds the legs.
//
// The leg ids are derived against `legWagerID`. For a single ticket that is the
// ticket's own id; for a round robin it is the FIRST ticket's, and
// [Service.placeRoundRobin] re-mints each combination's legs under its own
// ticket id — migrations/00006 requires a distinct LegID per (ticket,
// selection) and explains why at length.
func (s *Service) bookLegs(
	ctx context.Context,
	tx Tx,
	slip Slip,
	legWagerID domain.WagerID,
	now time.Time,
) ([]domain.Leg, error) {
	legs := make([]domain.Leg, 0, len(slip.Legs))
	markets := make(map[domain.MarketID]struct{}, len(slip.Legs))

	for _, slipLeg := range slip.Legs {
		quote, err := tx.QuoteFor(ctx, slipLeg.SelectionID, slipLeg.BookID)
		if err != nil {
			return nil, fmt.Errorf("betting: quote %s at %s: %w",
				slipLeg.SelectionID, slipLeg.BookID, err)
		}

		// Staleness before the price comparison: a quote nobody has refreshed
		// is not a current line, so "it matches what you saw" is not the
		// interesting fact about it.
		if quote.Price.IsStale(now, s.maxQuoteAge) {
			return nil, fmt.Errorf("betting: quote for %s is %s old, the limit is %s: %w",
				slipLeg.SelectionID, quote.Price.Age(now), s.maxQuoteAge, ErrStaleQuote)
		}

		if move := checkPriceMove(slipLeg, quote.Price, slip.AcceptBetterPrice); move != nil {
			return nil, move
		}

		state, err := tx.MarketState(ctx, quote.MarketID)
		if err != nil {
			return nil, fmt.Errorf("betting: market %s: %w", quote.MarketID, err)
		}
		if !state.Status.AcceptsWagers() {
			return nil, fmt.Errorf("betting: market %s is %s: %w",
				state.MarketID, state.Status, ErrMarketNotOpen)
		}
		if !state.EventStatus.AcceptsWagers() {
			// The name is the common case; the check is the general one. Note
			// that a LIVE event passes — in-play betting is a feature
			// (CLAUDE.md §6) — so comparing the wall clock against
			// state.ScheduledStart here would refuse every live wager.
			return nil, fmt.Errorf("betting: event %s is %s: %w",
				state.EventID, state.EventStatus, ErrEventStarted)
		}

		if _, dup := markets[quote.MarketID]; dup {
			// legs_wager_market_key would refuse this at COMMIT; refusing it
			// here turns a 23505 into a sentence about competing answers to one
			// question. It cannot be checked in [Slip.Validate] because a slip
			// names selections and the market comes from the quote.
			return nil, fmt.Errorf("betting: selection %s and an earlier one both answer market %s: %w",
				slipLeg.SelectionID, quote.MarketID, ErrDuplicateMarket)
		}
		markets[quote.MarketID] = struct{}{}

		leg, err := s.buildLeg(legWagerID, slip, slipLeg, quote)
		if err != nil {
			return nil, err
		}
		legs = append(legs, leg)
	}
	return legs, nil
}

// buildLeg turns one re-read quote into a booked leg.
//
// THE PRICE IS quote.Price AND NOTHING ELSE. Not a Price constructed from the
// slip, not a Price with a substituted line — the store's own value, read under
// the transaction's snapshot. That is the invariant doc.go opens with, and it
// is enforced here by there being no other expression available.
func (s *Service) buildLeg(w domain.WagerID, slip Slip, slipLeg SlipLeg, quote Quote) (domain.Leg, error) {
	legID, err := DeriveLegID(w, slipLeg.SelectionID)
	if err != nil {
		return domain.Leg{}, err
	}

	params := domain.LegParams{
		ID:          legID,
		EventID:     quote.EventID,
		MarketID:    quote.MarketID,
		MarketType:  quote.MarketType,
		Role:        quote.Role,
		SelectionID: slipLeg.SelectionID,
		Price:       quote.Price,
	}

	if slip.Kind == domain.WagerKindTeaser {
		if quote.MarketType != domain.MarketTypeSpread && quote.MarketType != domain.MarketTypeTotal {
			return domain.Leg{}, fmt.Errorf("betting: selection %s is on a %s market: %w",
				slipLeg.SelectionID, quote.MarketType, ErrTeaserMarketType)
		}
		teased, err := teasedLine(quote.Role, quote.Price.Line(), slip.TeaserPoints)
		if err != nil {
			return domain.Leg{}, err
		}
		params.TeasedLine = teased
	}

	leg, err := domain.NewLeg(params)
	if err != nil {
		return domain.Leg{}, fmt.Errorf("betting: build leg for %s: %w", slipLeg.SelectionID, err)
	}
	return leg, nil
}

// placeSingle books a straight, a parlay or a teaser: one ticket, one stake
// transaction.
func (s *Service) placeSingle(
	ctx context.Context,
	tx Tx,
	req PlaceRequest,
	ids derivedIDs,
	booked []domain.Leg,
	now time.Time,
) (Placement, error) {
	id := ids.wagers[0]

	accepted, err := s.priceTicket(ctx, req.Slip, booked)
	if err != nil {
		return Placement{}, err
	}

	wager, err := domain.NewWager(domain.WagerParams{
		ID:              id,
		UserID:          req.UserID,
		Kind:            req.Slip.Kind,
		Legs:            booked,
		Stake:           req.Slip.Stake,
		AcceptedDecimal: accepted,
		Rounding:        req.Slip.Rounding,
		TeaserPoints:    req.Slip.TeaserPoints,
		PlacedAt:        now,
	})
	if err != nil {
		return Placement{}, fmt.Errorf("betting: build wager %s: %w", id, err)
	}

	written, replayed, err := s.writeTicket(ctx, tx, req.UserID, wager, now, req.Audit)
	if err != nil {
		return Placement{}, err
	}
	return Placement{Wagers: []domain.Wager{written}, Replayed: replayed}, nil
}

// placeRoundRobin books a round robin: the parent, then one ticket and one
// stake transaction per combination, all in this transaction.
//
// The expansion is domain.RoundRobin.Combinations()'s, not a second
// implementation of it — wager.go: "A '3-team round robin by 2s' is not one
// bet: it is three independent two-leg parlays ... Modelling it as one wager
// would make 'how much did ticket AC return' unanswerable."
//
// Each combination's legs are RE-MINTED under that combination's own ticket id.
// Combinations() returns subsets of the same []Leg values, so leg AB.a and leg
// AC.a arrive carrying one LegID, and migrations/00006 requires a distinct
// LegID per (ticket, selection) or the second INSERT violates legs_pkey.
func (s *Service) placeRoundRobin(
	ctx context.Context,
	tx Tx,
	req PlaceRequest,
	ids derivedIDs,
	booked []domain.Leg,
	now time.Time,
) (Placement, error) {
	parent, err := domain.NewRoundRobin(domain.RoundRobinParams{
		ID:                  ids.roundRobin,
		UserID:              req.UserID,
		Legs:                booked,
		Sizes:               req.Slip.Sizes,
		StakePerCombination: req.Slip.Stake,
		PlacedAt:            now,
	})
	if err != nil {
		return Placement{}, fmt.Errorf("betting: build round robin %s: %w", ids.roundRobin, err)
	}

	combinations := parent.Combinations()
	if len(combinations) != len(ids.wagers) {
		// [Slip.TicketCount] and RoundRobin.CombinationCount() disagreeing is a
		// bug in this package, not a customer error, and it is caught rather
		// than papered over: the derived ids were minted against one count and
		// the stake was multiplied by the same one, so a mismatch means the
		// customer would be charged for a different number of tickets than they
		// receive.
		return Placement{}, fmt.Errorf(
			"betting: round robin %s expands into %d tickets but %d ids were derived",
			parent.ID(), len(combinations), len(ids.wagers))
	}

	placement := Placement{
		Wagers:     make([]domain.Wager, 0, len(combinations)),
		RoundRobin: parent,
	}

	if err := tx.InsertRoundRobin(ctx, parent); err != nil {
		if errors.Is(err, ErrAlreadyPlaced) {
			// The parent already exists, so this is a replay whose first insert
			// got as far as the parent. Fall through to the per-ticket inserts,
			// each of which reports its own duplicate and reads back. The
			// parent is not re-read: it is immutable (round_robins has no
			// updated_at and no state machine) so the value built above is
			// byte-identical to the stored one.
			s.log.Info("round robin parent already exists; replaying",
				slog.String("round_robin_id", parent.ID().String()))
		} else {
			return Placement{}, fmt.Errorf("betting: insert round robin %s: %w", parent.ID(), err)
		}
	}

	anyReplayed := false
	for i, legs := range combinations {
		id := ids.wagers[i]

		reminted := make([]domain.Leg, len(legs))
		for j, leg := range legs {
			legID, err := DeriveLegID(id, leg.SelectionID())
			if err != nil {
				return Placement{}, err
			}
			reminted[j], err = domain.NewLeg(domain.LegParams{
				ID:          legID,
				EventID:     leg.EventID(),
				MarketID:    leg.MarketID(),
				MarketType:  leg.MarketType(),
				Role:        leg.Role(),
				SelectionID: leg.SelectionID(),
				Price:       leg.Price(),
			})
			if err != nil {
				return Placement{}, fmt.Errorf("betting: re-mint leg %s for ticket %s: %w",
					leg.SelectionID(), id, err)
			}
		}

		// Every combination is itself a parlay and is priced as one. It is
		// priced as WagerKindParlay rather than as WagerKindRoundRobin because
		// that is what it is — the round-robin kind describes how the ticket
		// came to exist, not how it pays.
		accepted, err := s.pricer.TicketDecimal(ctx, Ticket{
			Kind: domain.WagerKindParlay,
			Legs: reminted,
		})
		if err != nil {
			return Placement{}, fmt.Errorf("betting: price round robin ticket %s: %w", id, err)
		}

		wager, err := domain.NewWager(domain.WagerParams{
			ID:              id,
			UserID:          req.UserID,
			Kind:            domain.WagerKindRoundRobin,
			Legs:            reminted,
			Stake:           req.Slip.Stake,
			AcceptedDecimal: accepted,
			Rounding:        req.Slip.Rounding,
			RoundRobinID:    parent.ID(),
			PlacedAt:        now,
		})
		if err != nil {
			return Placement{}, fmt.Errorf("betting: build round robin ticket %s: %w", id, err)
		}

		written, replayed, err := s.writeTicket(ctx, tx, req.UserID, wager, now, req.Audit)
		if err != nil {
			return Placement{}, err
		}
		anyReplayed = anyReplayed || replayed
		placement.Wagers = append(placement.Wagers, written)
	}

	placement.Replayed = anyReplayed
	return placement, nil
}

// priceTicket computes the ticket price and applies the price-move rule to it.
//
// The customer never determines this number: it comes from the [TicketPricer],
// computed over legs built from quotes re-read in this transaction, and the
// slip's SeenTicketDecimal is only ever the left-hand side of a comparison.
func (s *Service) priceTicket(ctx context.Context, slip Slip, legs []domain.Leg) (float64, error) {
	accepted, err := s.pricer.TicketDecimal(ctx, Ticket{
		Kind:         slip.Kind,
		Legs:         legs,
		TeaserPoints: slip.TeaserPoints,
	})
	if err != nil {
		return 0, fmt.Errorf("betting: price ticket: %w", err)
	}
	if move := checkTicketPriceMove(slip, accepted); move != nil {
		return 0, move
	}
	return accepted, nil
}

// writeTicket inserts one wager and its stake transaction, and turns a
// primary-key collision into a read-back.
//
// This is the idempotency mechanism, in eight lines. Everything else in this
// package exists so that reaching here with a duplicate id means the same
// request arrived twice — see doc.go.
//
// The stake transaction is written AFTER the wager, which the foreign key
// requires (ledger_transactions.wager_id references wagers.id), and both are in
// this transaction so a ticket cannot exist without the money movement that
// paid for it. The double-entry assertion is deferred to COMMIT, so a
// mis-balanced movement fails there rather than here — which is precisely why
// [Store.InTx] must go through postgres.InTx.
//
// # The audit row, and why a replay does not get one
//
// The third write is the audit entry, in this same transaction, so the ticket,
// the money that paid for it and the record of who booked it commit together or
// none of them does (CLAUDE.md §6, and [Tx.RecordAudit]).
//
// It is written ONLY on the branch that actually inserted. A replay wrote
// nothing — it read an existing ticket back — and an audit row saying "this
// request placed this wager" would be false, would double-count the placement
// for anyone summing the trail, and would make a retrying client look like a
// customer betting repeatedly. The rule is that the trail records CHANGES, and
// a replay is by construction not one.
func (s *Service) writeTicket(
	ctx context.Context,
	tx Tx,
	user domain.UserID,
	wager domain.Wager,
	now time.Time,
	ac AuditContext,
) (domain.Wager, bool, error) {
	if err := tx.InsertWager(ctx, wager); err != nil {
		if !errors.Is(err, ErrAlreadyPlaced) {
			return domain.Wager{}, false, fmt.Errorf("betting: insert wager %s: %w", wager.ID(), err)
		}
		existing, err := s.readBackOne(ctx, tx, user, wager.ID())
		if err != nil {
			return domain.Wager{}, false, err
		}
		// The stake transaction is NOT re-written. It was written in the same
		// transaction as the original wager, so it exists exactly when the
		// wager does, and an idempotent "insert if missing" here would be a
		// second money path with no test that ever exercises it. The audit row
		// is not re-written either, and for the stronger reason above.
		return existing, true, nil
	}

	txnID, err := DeriveTransactionID(wager.ID(), domain.EntryKindStake)
	if err != nil {
		return domain.Wager{}, false, err
	}
	stake, err := domain.NewStakeTransaction(txnID, wager, now)
	if err != nil {
		return domain.Wager{}, false, fmt.Errorf("betting: build stake transaction for %s: %w", wager.ID(), err)
	}
	if err := tx.InsertTransaction(ctx, stake); err != nil {
		return domain.Wager{}, false, fmt.Errorf("betting: insert stake transaction %s: %w", txnID, err)
	}
	if err := auditPlacement(ctx, tx, wager, ac); err != nil {
		// Returned, not logged and swallowed. A failed audit write aborts the
		// placement — see [Tx.RecordAudit]: a booked bet with no record of who
		// booked it is the outcome the requirement exists to prevent, and a
		// refused bet is the recoverable half of the pair.
		return domain.Wager{}, false, err
	}
	return wager, false, nil
}

// readBack rehydrates a set of already-placed wagers.
func (s *Service) readBack(ctx context.Context, tx Tx, user domain.UserID, ids []domain.WagerID) ([]domain.Wager, error) {
	wagers := make([]domain.Wager, len(ids))
	for i, id := range ids {
		w, err := s.readBackOne(ctx, tx, user, id)
		if err != nil {
			return nil, err
		}
		wagers[i] = w
	}
	return wagers, nil
}

// readBackOne rehydrates one already-placed wager and asserts it belongs to the
// customer asking for it.
//
// The ownership assertion is defence in depth rather than a real control: the
// user id is inside the hash, so two customers cannot derive one wager id
// without a SHA-256 collision. It is checked anyway because the cost is one
// comparison and the failure it guards against — returning one customer's
// ticket to another — is the worst outcome available on this path, and the kind
// of thing that would be introduced later by someone "simplifying" the
// derivation to drop the user id.
func (s *Service) readBackOne(ctx context.Context, tx Tx, user domain.UserID, id domain.WagerID) (domain.Wager, error) {
	w, err := tx.WagerByID(ctx, id)
	if err != nil {
		return domain.Wager{}, fmt.Errorf("betting: read back wager %s after a duplicate insert: %w", id, err)
	}
	if w.UserID() != user {
		return domain.Wager{}, fmt.Errorf("betting: wager %s belongs to another account: %w", id, ErrWagerNotFound)
	}
	return w, nil
}
