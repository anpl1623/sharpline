package betting

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/domain"
)

func TestNewServiceRefusesBadOptions(t *testing.T) {
	t.Parallel()

	store := &fakeStore{tx: newFakeTx()}

	tests := []struct {
		name   string
		store  Store
		pricer TicketPricer
		clock  Clock
		opts   Options
	}{
		{name: "no store", pricer: IndependentPricer{}, clock: testClock},
		{name: "no pricer", store: store, clock: testClock},
		{name: "no clock", store: store, pricer: IndependentPricer{}},
		{
			// A negative horizon makes every quote stale and refuses every bet.
			// It has to fail at startup, not at 3am.
			name: "a negative quote age", store: store, pricer: IndependentPricer{}, clock: testClock,
			opts: Options{MaxQuoteAge: -time.Second},
		},
		{
			name: "a negative fair price age", store: store, pricer: IndependentPricer{}, clock: testClock,
			opts: Options{MaxFairPriceAge: -time.Second},
		},
		{
			name: "a negative idempotency ttl", store: store, pricer: IndependentPricer{}, clock: testClock,
			opts: Options{IdempotencyTTL: -time.Second},
		},
		{
			name: "a margin past the ceiling", store: store, pricer: IndependentPricer{}, clock: testClock,
			opts: Options{CashOutMarginBps: MaxCashOutMarginBps + 1},
		},
		{
			name: "a negative margin", store: store, pricer: IndependentPricer{}, clock: testClock,
			opts: Options{CashOutMarginBps: -1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewService(tc.store, tc.pricer, tc.clock, tc.opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("NewService() = %v, want ErrInvalidOptions", err)
			}
		})
	}
}

func TestNewServiceZeroMarginNeedsTheOptIn(t *testing.T) {
	t.Parallel()

	// "Zero means the default" and "zero is a legal value" cannot both be true
	// of one int field, so a deliberate zero margin is expressed explicitly.
	store := &fakeStore{tx: newFakeTx()}

	defaulted, err := NewService(store, IndependentPricer{}, testClock, Options{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if defaulted.cashOutMarginBps != DefaultCashOutMarginBps {
		t.Errorf("margin = %d, want the default %d", defaulted.cashOutMarginBps, DefaultCashOutMarginBps)
	}

	promotional, err := NewService(store, IndependentPricer{}, testClock, Options{ZeroCashOutMargin: true})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if promotional.cashOutMarginBps != 0 {
		t.Errorf("margin = %d, want an explicit 0", promotional.cashOutMarginBps)
	}
}

func TestPlaceStraight(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	svc, _ := newTestService(t, tx)

	placement, err := svc.Place(context.Background(), PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip:           straightSlip(quote, domain.Money(10_00)),
	})
	if err != nil {
		t.Fatalf("Place() = %v", err)
	}
	if placement.Replayed {
		t.Error("a first placement reported itself as a replay")
	}
	if len(placement.Wagers) != 1 {
		t.Fatalf("placed %d wagers, want 1", len(placement.Wagers))
	}

	w := placement.Wagers[0]
	if w.Kind() != domain.WagerKindStraight {
		t.Errorf("Kind = %s, want straight", w.Kind())
	}
	if w.Stake() != domain.Money(10_00) {
		t.Errorf("Stake = %s, want %s", w.Stake(), domain.Money(10_00))
	}
	// domain.validateTicketPrice requires exact equality for a straight; the
	// pricer returns the leg's own price, so this is the identity holding.
	if w.AcceptedDecimal() != 2.5 {
		t.Errorf("AcceptedDecimal = %g, want 2.5", w.AcceptedDecimal())
	}
	if w.PotentialPayout() != domain.Money(25_00) {
		t.Errorf("PotentialPayout = %s, want %s", w.PotentialPayout(), domain.Money(25_00))
	}
	if !w.PlacedAt().Equal(testNow) {
		t.Errorf("PlacedAt = %s, want the service's single instant %s", w.PlacedAt(), testNow)
	}

	// THE INVARIANT doc.go OPENS WITH: the booked price is the store's own
	// value, observation instant included, not one rebuilt from the slip.
	booked := w.Legs()[0].Price()
	if !booked.Equal(quote.Price) {
		t.Errorf("booked price %v is not the store's own value %v", booked, quote.Price)
	}

	// One ticket, one balanced stake movement, written in the same transaction.
	if len(tx.transactions) != 1 {
		t.Fatalf("wrote %d ledger transactions, want 1", len(tx.transactions))
	}
	stake := tx.transactions[0]
	if stake.Kind() != domain.EntryKindStake {
		t.Errorf("transaction kind = %s, want stake", stake.Kind())
	}
	if id, ok := stake.WagerID(); !ok || id != w.ID() {
		t.Errorf("transaction names wager %s, want %s", id, w.ID())
	}
	if err := domain.LedgerIsBalanced(stake); err != nil {
		t.Errorf("the stake movement does not balance: %v", err)
	}
}

func TestPlaceParlay(t *testing.T) {
	t.Parallel()

	one := moneylineQuote(t, 1, 2.0)
	two := moneylineQuote(t, 2, 1.8)
	tx := newFakeTx().withQuote(one).withQuote(two)
	svc, _ := newTestService(t, tx)

	placement, err := svc.Place(context.Background(), PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip: Slip{
			Kind: domain.WagerKindParlay,
			Legs: []SlipLeg{
				{SelectionID: one.Price.SelectionID(), BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()},
				{SelectionID: two.Price.SelectionID(), BookID: testBook, SeenDecimal: 1.8, SeenLine: domain.NoLine()},
			},
			Stake:             domain.Money(10_00),
			SeenTicketDecimal: 3.6,
			Rounding:          domain.RoundHalfAwayFromZero,
		},
	})
	if err != nil {
		t.Fatalf("Place() = %v", err)
	}

	w := placement.Wagers[0]
	if w.LegCount() != 2 {
		t.Fatalf("LegCount = %d, want 2", w.LegCount())
	}
	if w.AcceptedDecimal() != 3.6 {
		t.Errorf("AcceptedDecimal = %g, want 3.6", w.AcceptedDecimal())
	}
	// The legs carry distinct derived ids even though they were built in one
	// pass — legs_pkey would refuse otherwise.
	legs := w.Legs()
	if legs[0].ID() == legs[1].ID() {
		t.Error("two legs of one ticket share an id")
	}
}

// TestPlaceRoundRobin is the expansion migrations/00006 and wager.go both
// insist on: N independent tickets, N stake movements, one parent, and a
// DISTINCT leg id per (ticket, selection).
func TestPlaceRoundRobin(t *testing.T) {
	t.Parallel()

	quotes := []Quote{moneylineQuote(t, 1, 2.0), moneylineQuote(t, 2, 2.0), moneylineQuote(t, 3, 2.0)}
	tx := newFakeTx()
	legs := make([]SlipLeg, len(quotes))
	for i, q := range quotes {
		tx.withQuote(q)
		legs[i] = SlipLeg{SelectionID: q.Price.SelectionID(), BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()}
	}
	svc, _ := newTestService(t, tx)

	placement, err := svc.Place(context.Background(), PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip: Slip{
			Kind:     domain.WagerKindRoundRobin,
			Legs:     legs,
			Sizes:    []int{2},
			Stake:    domain.Money(5_00),
			Rounding: domain.RoundHalfAwayFromZero,
		},
	})
	if err != nil {
		t.Fatalf("Place() = %v", err)
	}

	// wager.go: "A '3-team round robin by 2s' is not one bet: it is three
	// independent two-leg parlays — AB, AC, BC."
	if len(placement.Wagers) != 3 {
		t.Fatalf("placed %d tickets, want C(3,2) = 3", len(placement.Wagers))
	}
	if placement.RoundRobin.IsZero() {
		t.Fatal("the placement carries no round robin parent")
	}
	if len(tx.roundRobins) != 1 {
		t.Errorf("wrote %d round robin parents, want 1", len(tx.roundRobins))
	}
	if len(tx.transactions) != 3 {
		t.Errorf("wrote %d stake movements, want one per ticket", len(tx.transactions))
	}

	// The requirement migrations/00006 states directly: Combinations() returns
	// subsets of the SAME []Leg values, so without re-minting, ticket AB's leg
	// `a` and ticket AC's leg `a` would share a LegID and the second INSERT
	// would violate legs_pkey.
	seen := map[domain.LegID]domain.WagerID{}
	for _, w := range placement.Wagers {
		if w.LegCount() != 2 {
			t.Errorf("ticket %s has %d legs, want 2", w.ID(), w.LegCount())
		}
		if id, ok := w.RoundRobinID(); !ok || id != placement.RoundRobin.ID() {
			t.Errorf("ticket %s names parent %s, want %s", w.ID(), id, placement.RoundRobin.ID())
		}
		if w.Stake() != domain.Money(5_00) {
			t.Errorf("ticket %s stakes %s, want the per-combination stake %s",
				w.ID(), w.Stake(), domain.Money(5_00))
		}
		for _, leg := range w.Legs() {
			if other, dup := seen[leg.ID()]; dup {
				t.Fatalf("leg id %s appears on both %s and %s", leg.ID(), other, w.ID())
			}
			seen[leg.ID()] = w.ID()
		}
	}

	// And the stake the customer was charged is the total, not one ticket's:
	// three tickets at $5 risks $15.
	total := domain.ZeroMoney
	for _, w := range placement.Wagers {
		total += w.Stake()
	}
	if total != domain.Money(15_00) {
		t.Errorf("total staked = %s, want %s", total, domain.Money(15_00))
	}
}

// TestPlaceIsIdempotent is the claim doc.go makes: the same key books one
// wager, and the replay returns the first one.
func TestPlaceIsIdempotent(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	svc, _ := newTestService(t, tx)

	req := PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip:           straightSlip(quote, domain.Money(10_00)),
	}

	first, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("first Place() = %v", err)
	}
	second, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("second Place() = %v", err)
	}

	if !second.Replayed {
		t.Error("the second placement did not report itself as a replay")
	}
	if first.Wagers[0].ID() != second.Wagers[0].ID() {
		t.Fatalf("the replay returned a different wager: %s vs %s",
			second.Wagers[0].ID(), first.Wagers[0].ID())
	}
	if len(tx.wagers) != 1 {
		t.Fatalf("stored %d wagers, want 1 — the customer was double-charged", len(tx.wagers))
	}
	// The stake movement is written once, in the same transaction as the
	// wager, so it exists exactly when the wager does.
	if len(tx.transactions) != 1 {
		t.Fatalf("wrote %d stake movements, want 1", len(tx.transactions))
	}
}

func TestPlaceIsIdempotentAcrossARoundRobin(t *testing.T) {
	t.Parallel()

	quotes := []Quote{moneylineQuote(t, 1, 2.0), moneylineQuote(t, 2, 2.0), moneylineQuote(t, 3, 2.0)}
	tx := newFakeTx()
	legs := make([]SlipLeg, len(quotes))
	for i, q := range quotes {
		tx.withQuote(q)
		legs[i] = SlipLeg{SelectionID: q.Price.SelectionID(), BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()}
	}
	svc, _ := newTestService(t, tx)

	req := PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip: Slip{
			Kind:     domain.WagerKindRoundRobin,
			Legs:     legs,
			Sizes:    []int{2},
			Stake:    domain.Money(5_00),
			Rounding: domain.RoundHalfAwayFromZero,
		},
	}

	if _, err := svc.Place(context.Background(), req); err != nil {
		t.Fatalf("first Place() = %v", err)
	}
	second, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("second Place() = %v", err)
	}

	if !second.Replayed {
		t.Error("the replayed round robin did not report itself as one")
	}
	if len(tx.wagers) != 3 {
		t.Fatalf("stored %d tickets, want 3 — the round robin was booked twice", len(tx.wagers))
	}
	if len(tx.transactions) != 3 {
		t.Fatalf("wrote %d stake movements, want 3", len(tx.transactions))
	}
}

// TestPlaceIdempotencyDoesNotDependOnRedis is the claim in doc.go that Redis is
// a shortcut: with the cache failing on every call, the outcome is identical.
func TestPlaceIdempotencyDoesNotDependOnRedis(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	cache := newFakeCache()
	cache.lookupErr = errors.New("redis: connection refused")
	cache.recordErr = errors.New("redis: connection refused")

	svc, _ := newTestService(t, tx, func(o *Options) { o.Cache = cache })

	req := PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip:           straightSlip(quote, domain.Money(10_00)),
	}

	first, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("first Place() with a dead cache = %v", err)
	}
	second, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("second Place() with a dead cache = %v", err)
	}
	if !second.Replayed || second.Wagers[0].ID() != first.Wagers[0].ID() {
		t.Fatal("a dead cache broke idempotency; Postgres was supposed to be the guard")
	}
	if len(tx.wagers) != 1 {
		t.Fatalf("stored %d wagers with a dead cache, want 1", len(tx.wagers))
	}
}

// TestPlaceFastPathSkipsTheWork asserts what the cache actually buys: a replay
// that hits it does not re-read the status, the quotes or the balance.
func TestPlaceFastPathSkipsTheWork(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	cache := newFakeCache()
	svc, _ := newTestService(t, tx, func(o *Options) { o.Cache = cache })

	req := PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip:           straightSlip(quote, domain.Money(10_00)),
	}

	if _, err := svc.Place(context.Background(), req); err != nil {
		t.Fatalf("first Place() = %v", err)
	}
	statusAfterFirst, quotesAfterFirst, balanceAfterFirst := tx.statusCalls, tx.quoteCalls, tx.balanceCalls

	replay, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("replay Place() = %v", err)
	}
	if !replay.Replayed {
		t.Fatal("the cached replay did not report itself as one")
	}
	if tx.statusCalls != statusAfterFirst {
		t.Errorf("the fast path re-read the user status (%d → %d)", statusAfterFirst, tx.statusCalls)
	}
	if tx.quoteCalls != quotesAfterFirst {
		t.Errorf("the fast path re-quoted (%d → %d)", quotesAfterFirst, tx.quoteCalls)
	}
	if tx.balanceCalls != balanceAfterFirst {
		t.Errorf("the fast path re-folded the balance (%d → %d)", balanceAfterFirst, tx.balanceCalls)
	}

	// And it still read the BODY back from Postgres rather than serving a
	// cached one — see IdempotencyCache on why ids and not bodies.
	if replay.Wagers[0].Status() != domain.WagerStatusPlaced {
		t.Errorf("replayed wager status = %s, want the stored one", replay.Wagers[0].Status())
	}
}

// TestPlaceRefusesSelfExcludedAccounts is the phase-5 open question answered.
// auth.UserStatus.CanWager argues at length that this check has to be here,
// inside the transaction that writes the wager, and not in HTTP middleware.
func TestPlaceRefusesByAccountStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status string
		want   error
	}{
		{name: "active places", status: auth.UserStatusActive.String()},
		{name: "self-excluded", status: auth.UserStatusSelfExcluded.String(), want: ErrSelfExcluded},
		{name: "suspended", status: auth.UserStatusSuspended.String(), want: ErrAccountNotWagerable},
		{name: "closed", status: auth.UserStatusClosed.String(), want: ErrAccountNotWagerable},
		{
			// FAILS CLOSED. A status this build does not recognise — because
			// the column's CHECK grew a value the binary predates — must not
			// read as "active", which is the one value that permits everything.
			name: "an unrecognised status", status: "pending_review", want: ErrAccountNotWagerable,
		},
		{name: "an empty status", status: "", want: ErrAccountNotWagerable},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			quote := moneylineQuote(t, 1, 2.5)
			tx := newFakeTx().withQuote(quote)
			tx.status = tc.status
			svc, _ := newTestService(t, tx)

			_, err := svc.Place(context.Background(), PlaceRequest{
				UserID:         testUser,
				IdempotencyKey: testKey,
				Slip:           straightSlip(quote, domain.Money(10_00)),
			})

			if tc.want == nil {
				if err != nil {
					t.Fatalf("Place() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Place() = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if !errors.Is(err, ErrNotPermitted) {
				t.Fatalf("Place() = %v, want it in the ErrNotPermitted class (403)", err)
			}
			// A refused placement writes NOTHING. The status check runs before
			// the quotes are read and long before anything is inserted.
			if len(tx.wagers) != 0 || len(tx.transactions) != 0 {
				t.Fatal("a refused placement wrote to the database")
			}
			// Self-exclusion keeps its own sentinel, because the response it
			// earns is different in kind from a suspension.
			if tc.want == ErrSelfExcluded && errors.Is(err, ErrAccountNotWagerable) {
				t.Error("self-exclusion collapsed into the generic not-wagerable refusal")
			}
		})
	}
}

func TestPlaceRefusesAPriceMove(t *testing.T) {
	t.Parallel()

	// The book is at 2.20; the customer saw 2.50.
	quote := moneylineQuote(t, 1, 2.20)
	tx := newFakeTx().withQuote(quote)
	svc, _ := newTestService(t, tx)

	slip := straightSlip(quote, domain.Money(10_00))
	slip.Legs[0].SeenDecimal = 2.50

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: slip,
	})
	if !errors.Is(err, ErrPriceMoved) {
		t.Fatalf("Place() = %v, want ErrPriceMoved", err)
	}
	if len(tx.wagers) != 0 {
		t.Fatal("a moved price was booked anyway")
	}

	// The bet slip has to render both numbers, so both have to survive as
	// values rather than as message text.
	var move *PriceMove
	if !errors.As(err, &move) {
		t.Fatalf("Place() = %v, want a *PriceMove a caller can render", err)
	}
	if move.SeenDecimal != 2.50 || move.CurrentDecimal != 2.20 {
		t.Fatalf("PriceMove = seen %g / current %g, want 2.5 / 2.2", move.SeenDecimal, move.CurrentDecimal)
	}
}

// TestPlaceAcceptsAMovedPriceWithAnAcceptance is the accept/reject round trip
// CLAUDE.md §6 asks for, and the other half of the price-move invariant: the
// refusal is not a wall, it is a re-quote.
func TestPlaceAcceptsAMovedPriceWithAnAcceptance(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.20)
	tx := newFakeTx().withQuote(quote)
	svc, _ := newTestService(t, tx)

	slip := straightSlip(quote, domain.Money(10_00))
	slip.Legs[0].SeenDecimal = 2.50
	slip.Legs[0].Accept = &Acceptance{Decimal: 2.20, Line: domain.NoLine()}

	placement, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: slip,
	})
	if err != nil {
		t.Fatalf("Place() with an acceptance = %v", err)
	}
	// Booked at the CURRENT price, not at the accepted number the client sent
	// — they are equal by the check, and using the store's value is what makes
	// "the customer determined the price" unrepresentable.
	if got := placement.Wagers[0].Legs()[0].Price().Decimal(); got != 2.20 {
		t.Fatalf("booked at %g, want the store's current 2.20", got)
	}
	if placement.Wagers[0].AcceptedDecimal() != 2.20 {
		t.Fatalf("ticket priced at %g, want 2.20", placement.Wagers[0].AcceptedDecimal())
	}
}

func TestPlaceRefusesMarketAndEventState(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		market     domain.MarketStatus
		event      domain.EventStatus
		observedAt time.Time
		want       error
		wantPlaced bool
	}{
		{
			name:   "an open market on a scheduled event places",
			market: domain.MarketStatusOpen, event: domain.EventStatusScheduled,
			wantPlaced: true,
		},
		{
			// In-play betting is a feature (CLAUDE.md §6), so a LIVE event must
			// place. A wall-clock comparison against the kickoff time here
			// would refuse every live wager.
			name:   "an open market on a LIVE event places",
			market: domain.MarketStatusOpen, event: domain.EventStatusLive,
			wantPlaced: true,
		},
		{
			name: "a suspended market", market: domain.MarketStatusSuspended, event: domain.EventStatusLive,
			want: ErrMarketNotOpen,
		},
		{
			name: "a closed market", market: domain.MarketStatusClosed, event: domain.EventStatusLive,
			want: ErrMarketNotOpen,
		},
		{
			name: "an ended event", market: domain.MarketStatusOpen, event: domain.EventStatusEnded,
			want: ErrEventStarted,
		},
		{
			name: "a cancelled event", market: domain.MarketStatusOpen, event: domain.EventStatusCancelled,
			want: ErrEventStarted,
		},
		{
			name: "a stale quote", market: domain.MarketStatusOpen, event: domain.EventStatusLive,
			observedAt: testNow.Add(-DefaultMaxQuoteAge - time.Second),
			want:       ErrStaleQuote,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			observed := testObserved
			if !tc.observedAt.IsZero() {
				observed = tc.observedAt
			}
			sel := domain.SelectionID("sel-1")
			quote := Quote{
				Price:      mustPrice(t, sel, testBook, 2.5, domain.NoLine(), observed),
				EventID:    "evt-1",
				MarketID:   "mkt-1",
				MarketType: domain.MarketTypeMoneyline,
				Role:       domain.SelectionRoleHome,
			}

			tx := newFakeTx()
			tx.quotes[sel] = quote
			tx.markets["mkt-1"] = MarketState{
				MarketID: "mkt-1", EventID: "evt-1",
				Status: tc.market, EventStatus: tc.event,
				ScheduledStart: testNow.Add(-time.Hour),
			}
			svc, _ := newTestService(t, tx)

			_, err := svc.Place(context.Background(), PlaceRequest{
				UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
			})
			if tc.wantPlaced {
				if err != nil {
					t.Fatalf("Place() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Place() = %v, want errors.Is(_, %v)", err, tc.want)
			}
			if !errors.Is(err, ErrMarketMoved) {
				t.Fatalf("Place() = %v, want it in the ErrMarketMoved class (409)", err)
			}
		})
	}
}

func TestPlaceRefusesAnUnfundedSlip(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	tx.balance = domain.Money(9_99)
	svc, _ := newTestService(t, tx)

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Place() = %v, want ErrInsufficientFunds", err)
	}
	if !errors.Is(err, ErrUnaffordable) {
		t.Fatalf("Place() = %v, want it in the ErrUnaffordable class (422), not the 403 one", err)
	}

	var short *ShortFall
	if !errors.As(err, &short) {
		t.Fatalf("Place() = %v, want a *ShortFall", err)
	}
	if short.Available != 999 || short.Required != 1000 {
		t.Fatalf("ShortFall = %+v, want available 999 / required 1000", short)
	}

	// The boundary: exactly enough places.
	tx.balance = domain.Money(10_00)
	if _, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
	}); err != nil {
		t.Fatalf("Place() with exactly the stake = %v, want nil", err)
	}
}

// TestPlaceChecksTheTOTAL stake of a round robin, not one ticket's — wager.go's
// "'$5 round robin by 2s' on four selections risks $30, not $5". Checking one
// ticket's stake would book six tickets against a balance that covers one.
func TestPlaceChecksTheTotalRoundRobinStake(t *testing.T) {
	t.Parallel()

	quotes := []Quote{moneylineQuote(t, 1, 2.0), moneylineQuote(t, 2, 2.0), moneylineQuote(t, 3, 2.0)}
	tx := newFakeTx()
	legs := make([]SlipLeg, len(quotes))
	for i, q := range quotes {
		tx.withQuote(q)
		legs[i] = SlipLeg{SelectionID: q.Price.SelectionID(), BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()}
	}
	// Enough for two of the three tickets, not for all three.
	tx.balance = domain.Money(10_00)
	svc, _ := newTestService(t, tx)

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey,
		Slip: Slip{
			Kind: domain.WagerKindRoundRobin, Legs: legs, Sizes: []int{2},
			Stake: domain.Money(5_00), Rounding: domain.RoundHalfAwayFromZero,
		},
	})
	if !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("Place() = %v, want ErrInsufficientFunds against the $15 total", err)
	}
	if len(tx.wagers) != 0 {
		t.Fatal("an unfunded round robin booked tickets anyway")
	}
}

func TestPlaceRefusesADuplicateMarket(t *testing.T) {
	t.Parallel()

	// Two DIFFERENT selections answering ONE market: home and away on the same
	// moneyline. They cannot both win, so the ticket is dead on arrival.
	// Slip.Validate cannot see this — a slip names selections and the market
	// comes from the quote.
	home := moneylineQuote(t, 1, 2.0)
	away := Quote{
		Price:      mustPrice(t, "sel-2", testBook, 1.8, domain.NoLine(), testObserved),
		EventID:    home.EventID,
		MarketID:   home.MarketID,
		MarketType: domain.MarketTypeMoneyline,
		Role:       domain.SelectionRoleAway,
	}

	tx := newFakeTx().withQuote(home)
	tx.quotes["sel-2"] = away
	svc, _ := newTestService(t, tx)

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey,
		Slip: Slip{
			Kind: domain.WagerKindParlay,
			Legs: []SlipLeg{
				{SelectionID: "sel-1", BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()},
				{SelectionID: "sel-2", BookID: testBook, SeenDecimal: 1.8, SeenLine: domain.NoLine()},
			},
			Stake: domain.Money(10_00), SeenTicketDecimal: 3.6, Rounding: domain.RoundHalfAwayFromZero,
		},
	})
	if !errors.Is(err, ErrDuplicateMarket) {
		t.Fatalf("Place() = %v, want ErrDuplicateMarket", err)
	}
}

// TestPlaceRefusesASameGameParlay is the refusal-to-misprice doc.go argues for.
// Pricing correlated legs as independent OVERPRICES the ticket, permanently,
// in the direction nobody audits.
func TestPlaceRefusesASameGameParlay(t *testing.T) {
	t.Parallel()

	one := moneylineQuote(t, 1, 2.0)
	two := Quote{
		Price:      mustPrice(t, "sel-2", testBook, 1.8, mustLine(t, 47.5), testObserved),
		EventID:    one.EventID, // the same game
		MarketID:   "mkt-2",
		MarketType: domain.MarketTypeTotal,
		Role:       domain.SelectionRoleOver,
	}

	tx := newFakeTx().withQuote(one)
	tx.quotes["sel-2"] = two
	tx.markets["mkt-2"] = MarketState{
		MarketID: "mkt-2", EventID: one.EventID,
		Status: domain.MarketStatusOpen, EventStatus: domain.EventStatusScheduled,
	}
	svc, _ := newTestService(t, tx)

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey,
		Slip: Slip{
			Kind: domain.WagerKindParlay,
			Legs: []SlipLeg{
				{SelectionID: "sel-1", BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()},
				{SelectionID: "sel-2", BookID: testBook, SeenDecimal: 1.8, SeenLine: mustLine(t, 47.5)},
			},
			Stake: domain.Money(10_00), SeenTicketDecimal: 3.6, Rounding: domain.RoundHalfAwayFromZero,
		},
	})
	if !errors.Is(err, ErrSameGameUnsupported) {
		t.Fatalf("Place() = %v, want ErrSameGameUnsupported", err)
	}
}

func TestPlaceRefusesATeaserWithoutALadder(t *testing.T) {
	t.Parallel()

	one := spreadQuote(t, 1, 1.91, -3.5)
	two := spreadQuote(t, 2, 1.91, 7.0)
	tx := newFakeTx().withQuote(one).withQuote(two)
	svc, _ := newTestService(t, tx)

	slip := Slip{
		Kind: domain.WagerKindTeaser,
		Legs: []SlipLeg{
			{SelectionID: "sel-1", BookID: testBook, SeenDecimal: 1.91, SeenLine: mustLine(t, -3.5)},
			{SelectionID: "sel-2", BookID: testBook, SeenDecimal: 1.91, SeenLine: mustLine(t, 7.0)},
		},
		Stake: domain.Money(10_00), TeaserPoints: 6, SeenTicketDecimal: 1.83,
		Rounding: domain.RoundHalfAwayFromZero,
	}

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: slip,
	})
	if !errors.Is(err, ErrTeaserUnsupported) {
		t.Fatalf("Place() = %v, want ErrTeaserUnsupported", err)
	}
}

// TestPlaceTeaserWithALadder exercises the whole teaser path, including the
// direction rule migrations/00006 had to add a database check for, using a
// pricer that supplies the posted ladder this package refuses to invent.
func TestPlaceTeaserWithALadder(t *testing.T) {
	t.Parallel()

	one := spreadQuote(t, 1, 1.91, -3.5)
	two := spreadQuote(t, 2, 1.91, 7.0)
	tx := newFakeTx().withQuote(one).withQuote(two)

	store := &fakeStore{tx: tx}
	svc, err := NewService(store, fixedTeaserPricer{decimal: 1.83}, testClock, Options{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	placement, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey,
		Slip: Slip{
			Kind: domain.WagerKindTeaser,
			Legs: []SlipLeg{
				{SelectionID: "sel-1", BookID: testBook, SeenDecimal: 1.91, SeenLine: mustLine(t, -3.5)},
				{SelectionID: "sel-2", BookID: testBook, SeenDecimal: 1.91, SeenLine: mustLine(t, 7.0)},
			},
			Stake: domain.Money(10_00), TeaserPoints: 6, SeenTicketDecimal: 1.83,
			Rounding: domain.RoundHalfAwayFromZero,
		},
	})
	if err != nil {
		t.Fatalf("Place() = %v", err)
	}

	w := placement.Wagers[0]
	if points, ok := w.TeaserPoints(); !ok || points != 6 {
		t.Fatalf("TeaserPoints = (%g, %v), want (6, true)", points, ok)
	}

	// Every leg carries a teased line, moved SIX POINTS IN THE BETTOR'S
	// FAVOUR. Both legs are home spreads here, so both move up — the direction
	// wagers_assert_shape() checks and domain.validateTeaser cannot see.
	want := map[domain.SelectionID]float64{"sel-1": 2.5, "sel-2": 13.0}
	for _, leg := range w.Legs() {
		v, ok := leg.TeasedLine().Value()
		if !ok {
			t.Fatalf("leg %s carries no teased line", leg.SelectionID())
		}
		if v != want[leg.SelectionID()] {
			t.Errorf("leg %s teased to %g, want %g", leg.SelectionID(), v, want[leg.SelectionID()])
		}
		// The booked price keeps the REAL market line beside the teased one, so
		// line history and CLV are not corrupted by a line the book never
		// traded (leg.go).
		if bookedLine, ok := leg.Price().Line().Value(); !ok || bookedLine == v {
			t.Errorf("leg %s booked line %g was overwritten by the tease", leg.SelectionID(), bookedLine)
		}
	}
}

func TestPlaceRefusesBadRequests(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)

	tests := []struct {
		name string
		req  PlaceRequest
		want error
	}{
		{
			name: "no user",
			req:  PlaceRequest{IdempotencyKey: testKey, Slip: straightSlip(quote, 1000)},
			want: domain.ErrEmptyID,
		},
		{
			// Required, not optional: without a key the wager id cannot be
			// derived, so a retried submit books a second bet.
			name: "no idempotency key",
			req:  PlaceRequest{UserID: testUser, Slip: straightSlip(quote, 1000)},
			want: ErrIdempotencyKeyRequired,
		},
		{
			name: "a whitespace-only key",
			req:  PlaceRequest{UserID: testUser, IdempotencyKey: "  ", Slip: straightSlip(quote, 1000)},
			want: ErrIdempotencyKeyRequired,
		},
		{
			name: "an invalid slip",
			req:  PlaceRequest{UserID: testUser, IdempotencyKey: testKey, Slip: Slip{}},
			want: ErrInvalidSlip,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tx := newFakeTx().withQuote(quote)
			svc, store := newTestService(t, tx)

			if _, err := svc.Place(context.Background(), tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("Place() = %v, want errors.Is(_, %v)", err, tc.want)
			}
			// A request refused before the transaction opens must not have
			// checked out a connection or taken the users row lock.
			if store.calls != 0 {
				t.Fatalf("a request refused by validation opened %d transactions", store.calls)
			}
		})
	}
}

// TestPlacePropagatesTheCommitError is the reason Store.InTx must go through
// postgres.InTx: the deferred zero-sum trigger fires at COMMIT, so every INSERT
// succeeds and the transaction fails afterwards. A helper that dropped that
// error would report a phantom money movement as written.
func TestPlacePropagatesTheCommitError(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx().withQuote(quote)
	store := &fakeStore{tx: tx, inTxErr: errors.New("postgres: commit: ledger transaction does not balance")}

	svc, err := NewService(store, IndependentPricer{}, testClock, Options{Logger: discardLogger()})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	_, err = svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
	})
	if err == nil {
		t.Fatal("Place() = nil, want the commit error; a rejected ledger write was reported as placed")
	}
	if !errors.Is(err, store.inTxErr) {
		t.Fatalf("Place() = %v, want it to wrap the commit error", err)
	}
}

func TestPlaceWrapsStoreFailures(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("connection reset by peer")

	tests := []struct {
		name  string
		setup func(*fakeTx)
	}{
		{name: "the status read", setup: func(tx *fakeTx) { tx.statusErr = sentinel }},
		{name: "the quote read", setup: func(tx *fakeTx) { tx.quoteErr = sentinel }},
		{name: "the balance fold", setup: func(tx *fakeTx) { tx.balanceErr = sentinel }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			quote := moneylineQuote(t, 1, 2.5)
			tx := newFakeTx().withQuote(quote)
			tc.setup(tx)
			svc, _ := newTestService(t, tx)

			_, err := svc.Place(context.Background(), PlaceRequest{
				UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("Place() = %v, want it to wrap %v", err, sentinel)
			}
			// An infrastructure failure must not read as a customer refusal:
			// telling somebody their price moved when the database was
			// unreachable sends them to retry a fix that is not the problem.
			for _, class := range []error{ErrInvalidSlip, ErrNotPermitted, ErrUnaffordable, ErrMarketMoved} {
				if errors.Is(err, class) {
					t.Fatalf("a store failure was classified as %v", class)
				}
			}
			if len(tx.wagers) != 0 {
				t.Fatal("a failed placement wrote a wager")
			}
		})
	}
}

func TestPlaceRefusesAnUnknownSelection(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 2.5)
	tx := newFakeTx() // no quotes registered
	svc, _ := newTestService(t, tx)

	_, err := svc.Place(context.Background(), PlaceRequest{
		UserID: testUser, IdempotencyKey: testKey, Slip: straightSlip(quote, domain.Money(10_00)),
	})
	if !errors.Is(err, ErrQuoteUnavailable) {
		t.Fatalf("Place() = %v, want ErrQuoteUnavailable", err)
	}
}

// TestSlipTicketCountAgreesWithTheDomain pins the one duplication slip.go
// admits to: Slip.binomial answers "would this overflow" before a transaction
// opens, and domain.RoundRobin.CombinationCount() is the authority used at
// placement. If they disagreed, the customer would be charged for a different
// number of tickets than they receive.
func TestSlipTicketCountAgreesWithTheDomain(t *testing.T) {
	t.Parallel()

	legs := make([]domain.Leg, domain.MaxRoundRobinLegs)
	slipLegs := make([]SlipLeg, domain.MaxRoundRobinLegs)
	for i := range legs {
		sel := domain.SelectionID(string(rune('a' + i)))
		price := mustPrice(t, sel, testBook, 2.0, domain.NoLine(), testObserved)
		leg, err := domain.NewLeg(domain.LegParams{
			ID:          domain.LegID("leg-" + string(rune('a'+i))),
			EventID:     domain.EventID("evt-" + string(rune('a'+i))),
			MarketID:    domain.MarketID("mkt-" + string(rune('a'+i))),
			MarketType:  domain.MarketTypeMoneyline,
			Role:        domain.SelectionRoleHome,
			SelectionID: sel,
			Price:       price,
		})
		if err != nil {
			t.Fatalf("NewLeg: %v", err)
		}
		legs[i] = leg
		slipLegs[i] = SlipLeg{SelectionID: sel, BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()}
	}

	for n := 2; n <= domain.MaxRoundRobinLegs; n++ {
		for k := 2; k <= n; k++ {
			slip := Slip{
				Kind: domain.WagerKindRoundRobin, Legs: slipLegs[:n], Sizes: []int{k},
				Stake: domain.Money(100), Rounding: domain.RoundHalfAwayFromZero,
			}
			got, err := slip.TicketCount()
			if err != nil {
				t.Fatalf("TicketCount(n=%d, k=%d) = %v", n, k, err)
			}

			rr, err := domain.NewRoundRobin(domain.RoundRobinParams{
				ID: "rrb-1", UserID: testUser, Legs: legs[:n], Sizes: []int{k},
				StakePerCombination: domain.Money(100), PlacedAt: testNow,
			})
			if err != nil {
				t.Fatalf("NewRoundRobin(n=%d, k=%d) = %v", n, k, err)
			}
			if want := rr.CombinationCount(); got != want {
				t.Fatalf("Slip.TicketCount(n=%d, k=%d) = %d, domain says %d", n, k, got, want)
			}
		}
	}
}

// TestAReplayIsAnsweredEvenAfterThePriceMoves is the fix for a contract
// violation the live stack produced: a retried submit was answered 409
// market_unavailable instead of with the ticket it had already booked.
//
// The mechanism was ordering. A replay used to be detected by the INSERT
// colliding with its derived primary key, which is a sound detector but is
// reached only AFTER the quotes are re-read and the price-move rule applied. A
// client retrying after a timeout retries seconds or minutes later, so its quote
// is very likely stale by then and the refusal fired first — in exactly the
// situation an idempotency key exists to resolve.
//
// The money was never at risk (the derived key still made a second booking
// impossible); what failed was the client's ability to learn that its first
// attempt had landed. This test drives that sequence directly: place, then move
// the price under the same key, and require the original ticket back.
func TestAReplayIsAnsweredEvenAfterThePriceMoves(t *testing.T) {
	t.Parallel()

	quote := moneylineQuote(t, 1, 1.91)
	tx := newFakeTx().withQuote(quote)
	svc, _ := newTestService(t, tx)

	req := PlaceRequest{
		UserID:         testUser,
		IdempotencyKey: testKey,
		Slip:           straightSlip(quote, domain.Money(2_500)),
	}

	first, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("first placement: %v", err)
	}
	if len(first.Wagers) != 1 {
		t.Fatalf("booked %d tickets, want 1", len(first.Wagers))
	}

	// The market moves. A fresh slip at the old price would now be refused.
	moved := moneylineQuote(t, 1, 2.50)
	tx.quotes[moved.Price.SelectionID()] = moved

	replay, err := svc.Place(context.Background(), req)
	if err != nil {
		t.Fatalf("a replay after a price move was refused with %v; the customer already "+
			"holds this ticket and cannot learn so", err)
	}
	if !replay.Replayed {
		t.Error("the replay did not report itself as one")
	}
	if len(replay.Wagers) != 1 {
		t.Fatalf("the replay returned %d tickets, want 1", len(replay.Wagers))
	}
	if replay.Wagers[0].ID() != first.Wagers[0].ID() {
		t.Errorf("the replay returned ticket %s, want %s", replay.Wagers[0].ID(), first.Wagers[0].ID())
	}
	// The booked price is the ORIGINAL one, not the moved one.
	if got, want := replay.Wagers[0].AcceptedDecimal(), first.Wagers[0].AcceptedDecimal(); got != want {
		t.Errorf("the replay reports price %v, want the originally booked %v", got, want)
	}
	if len(tx.wagers) != 1 {
		t.Errorf("%d tickets exist, want 1", len(tx.wagers))
	}
}
