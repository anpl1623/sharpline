package betting

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

func TestQuoteCashOut(t *testing.T) {
	t.Parallel()

	pending := func(fair float64) CashOutLeg {
		return CashOutLeg{SelectionID: "sel-1", Status: domain.LegStatusPending, Fair: odds.Decimal(fair)}
	}

	tests := []struct {
		name      string
		params    CashOutParams
		wantValue domain.Money
		wantFair  domain.Money
		wantErr   error
	}{
		{
			// The worked example from cashout.go's recorded disagreement: a $10
			// straight on a coinflip booked at 2.00, fair 2.00. The position is
			// worth the stake, so the offer is the stake less 5%.
			//
			// The brief's formula would return $19.00 here — nearly the full
			// winning payout, handed over while the game is still level.
			name: "a fair coinflip at fair odds is worth the stake less the haircut",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(2.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantFair:  domain.Money(10_00),
			wantValue: domain.Money(9_50),
		},
		{
			// A leg that has drifted against the customer is worth less; one
			// that has drifted for them is worth more. That is the whole point
			// of quoting off the CURRENT fair price rather than the booked one.
			name: "a leg that has drifted against the customer is worth less",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(4.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantFair:  domain.Money(5_00),
			wantValue: domain.Money(4_75),
		},
		{
			name: "a leg that has drifted for the customer is worth more",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(1.25)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantFair:  domain.Money(16_00),
			wantValue: domain.Money(15_20),
		},
		{
			name: "a two-leg parlay multiplies the survival probabilities",
			params: CashOutParams{
				PotentialPayout: domain.Money(40_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(2.0), pending(2.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantFair:  domain.Money(10_00), // 4000 × 0.5 × 0.5
			wantValue: domain.Money(9_50),
		},
		{
			// A won leg is already in, so it contributes a factor of one and
			// the remaining pending leg carries the whole probability.
			name: "a won leg contributes a factor of one",
			params: CashOutParams{
				PotentialPayout: domain.Money(40_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs: []CashOutLeg{
					{SelectionID: "sel-1", Status: domain.LegStatusWon},
					pending(2.0),
				},
				MarginBps: DefaultCashOutMarginBps,
			},
			wantFair:  domain.Money(20_00),
			wantValue: domain.Money(19_00),
		},
		{
			name: "a zero margin gives the whole fair value",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(2.0)},
				MarginBps:       0,
			},
			wantFair:  domain.Money(10_00),
			wantValue: domain.Money(10_00),
		},
		{
			name: "a lost leg makes the ticket worthless and is refused",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs: []CashOutLeg{
					{SelectionID: "sel-1", Status: domain.LegStatusLost},
					pending(2.0),
				},
				MarginBps: DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			// The refusal worth defending: a voided leg drops out of a parlay,
			// which REPRICES the ticket, so PotentialPayout is no longer the
			// promise. Quoting off a payout known to be wrong is the "plausible
			// number of the right magnitude" failure.
			name: "a void leg is refused because the ticket needs repricing",
			params: CashOutParams{
				PotentialPayout: domain.Money(40_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs: []CashOutLeg{
					{SelectionID: "sel-1", Status: domain.LegStatusVoid},
					pending(2.0),
				},
				MarginBps: DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			name: "a pushed leg is refused for the same reason",
			params: CashOutParams{
				PotentialPayout: domain.Money(40_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs: []CashOutLeg{
					{SelectionID: "sel-1", Status: domain.LegStatusPush},
					pending(2.0),
				},
				MarginBps: DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			name: "a pending leg with no fair price is refused, never approximated",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{{SelectionID: "sel-1", Status: domain.LegStatusPending}},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			name: "a NaN fair price is refused",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(math.NaN())},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			// A very long fair price collapses the survival probability, and a
			// quote of zero is not an offer.
			name: "a value that rounds to nothing is refused",
			params: CashOutParams{
				PotentialPayout: domain.Money(2),
				Rounding:        domain.RoundTowardZero,
				Legs:            []CashOutLeg{pending(1000.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			name: "no legs is refused",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			name: "a non-positive payout is refused",
			params: CashOutParams{
				PotentialPayout: 0,
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(2.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: ErrCashOutUnavailable,
		},
		{
			// money.go: "a silent default is how a house edge appears in a
			// ledger that nobody meant to put one in".
			name: "no rounding mode is refused rather than defaulted",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Legs:            []CashOutLeg{pending(2.0)},
				MarginBps:       DefaultCashOutMarginBps,
			},
			wantErr: domain.ErrUnknownRounding,
		},
		{
			// The units-error guard: a margin passed as a percentage (5) or as
			// a fraction (0.05 truncated to 0) both read as almost no margin,
			// so the ceiling is paired with the positivity check in NewService.
			name: "a margin past the ceiling is refused",
			params: CashOutParams{
				PotentialPayout: domain.Money(20_00),
				Rounding:        domain.RoundHalfAwayFromZero,
				Legs:            []CashOutLeg{pending(2.0)},
				MarginBps:       MaxCashOutMarginBps + 1,
			},
			wantErr: ErrInvalidOptions,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := QuoteCashOut(tc.params)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("QuoteCashOut() = (%+v, %v), want errors.Is(_, %v)", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("QuoteCashOut() = %v", err)
			}
			if got.Value != tc.wantValue {
				t.Errorf("Value = %s, want %s", got.Value, tc.wantValue)
			}
			if got.FairValue != tc.wantFair {
				t.Errorf("FairValue = %s, want %s", got.FairValue, tc.wantFair)
			}
			// The reason the margin is named rather than buried in a devig: the
			// take is a subtraction the customer can see.
			if got.FairValue.Compare(got.Value) < 0 {
				t.Errorf("FairValue %s is below Value %s; the haircut has the wrong sign",
					got.FairValue, got.Value)
			}
			if got.MarginBps != tc.params.MarginBps {
				t.Errorf("MarginBps = %d, want %d", got.MarginBps, tc.params.MarginBps)
			}
		})
	}
}

// TestQuoteCashOutNeverExceedsThePayout pins the bound wager.go calls the one
// that matters: "a return above the maximum is an arithmetic fault caught here
// rather than an overpayment discovered in a reconciliation". Catching it at
// the quote means the customer is never SHOWN a number the ticket cannot pay.
func TestQuoteCashOutNeverExceedsThePayout(t *testing.T) {
	t.Parallel()

	// A fair price at the very floor of the representable range implies a near
	// certainty, which drives the fair value up towards the whole payout.
	for _, fair := range []float64{1.0001, 1.01, 1.5, 2.0, 10.0} {
		quote, err := QuoteCashOut(CashOutParams{
			PotentialPayout: domain.Money(100_00),
			Rounding:        domain.RoundHalfAwayFromZero,
			Legs:            []CashOutLeg{{SelectionID: "sel-1", Status: domain.LegStatusPending, Fair: odds.Decimal(fair)}},
			MarginBps:       0,
		})
		if err != nil {
			t.Fatalf("QuoteCashOut(fair=%g) = %v", fair, err)
		}
		if quote.Value.Compare(domain.Money(100_00)) > 0 {
			t.Fatalf("fair=%g quoted %s against a %s payout", fair, quote.Value, domain.Money(100_00))
		}
		if !quote.Value.IsPositive() {
			t.Fatalf("fair=%g quoted a non-positive value %s", fair, quote.Value)
		}
	}
}

// TestQuoteCashOutUsesTheTicketsRounding asserts that the mode comes from the
// ticket rather than being chosen fresh — wager.go: "a later recomputation ...
// uses the same rule the ticket was written under rather than picking a fresh
// one".
func TestQuoteCashOutUsesTheTicketsRounding(t *testing.T) {
	t.Parallel()

	// A payout and fair price chosen so the fair value lands exactly on a half
	// minor unit, where the three modes disagree: 1001 × (1/2) = 500.5.
	params := func(r domain.Rounding) CashOutParams {
		return CashOutParams{
			PotentialPayout: domain.Money(1001),
			Rounding:        r,
			Legs:            []CashOutLeg{{SelectionID: "sel-1", Status: domain.LegStatusPending, Fair: odds.Decimal(2.0)}},
			MarginBps:       0,
		}
	}

	tests := []struct {
		rounding domain.Rounding
		want     domain.Money
	}{
		{rounding: domain.RoundHalfAwayFromZero, want: 501},
		{rounding: domain.RoundHalfToEven, want: 500},
		{rounding: domain.RoundTowardZero, want: 500},
	}

	for _, tc := range tests {
		t.Run(tc.rounding.String(), func(t *testing.T) {
			t.Parallel()
			got, err := QuoteCashOut(params(tc.rounding))
			if err != nil {
				t.Fatalf("QuoteCashOut() = %v", err)
			}
			if got.FairValue != tc.want {
				t.Fatalf("FairValue under %s = %s, want %s", tc.rounding, got.FairValue, tc.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// The service method
// -----------------------------------------------------------------------------

// cashOutFixture builds a placed, still-running straight and the service that
// can quote it.
func cashOutFixture(t *testing.T, fairObservedAt time.Time) (*Service, domain.Wager, *fakeFairPrices) {
	t.Helper()

	quote := moneylineQuote(t, 1, 2.0)
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          "leg-1",
		EventID:     quote.EventID,
		MarketID:    quote.MarketID,
		MarketType:  quote.MarketType,
		Role:        quote.Role,
		SelectionID: quote.Price.SelectionID(),
		Price:       quote.Price,
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	wager, err := domain.NewWager(domain.WagerParams{
		ID:              "wgr-1",
		UserID:          testUser,
		Kind:            domain.WagerKindStraight,
		Legs:            []domain.Leg{leg},
		Stake:           domain.Money(10_00),
		AcceptedDecimal: 2.0,
		Rounding:        domain.RoundHalfAwayFromZero,
		PlacedAt:        testNow.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("NewWager: %v", err)
	}

	fair := &fakeFairPrices{prices: map[domain.SelectionID]FairPrice{
		quote.Price.SelectionID(): {
			SelectionID: quote.Price.SelectionID(),
			Decimal:     odds.Decimal(2.0),
			ObservedAt:  fairObservedAt,
		},
	}}

	svc, _ := newTestService(t, newFakeTx(), func(o *Options) {
		o.Wagers = &fakeWagers{wagers: map[domain.WagerID]domain.Wager{wager.ID(): wager}}
		o.FairPrices = fair
	})
	return svc, wager, fair
}

func TestServiceCashOutQuote(t *testing.T) {
	t.Parallel()

	svc, wager, _ := cashOutFixture(t, testNow.Add(-time.Second))

	quote, err := svc.CashOutQuote(context.Background(), wager.ID())
	if err != nil {
		t.Fatalf("CashOutQuote() = %v", err)
	}
	if quote.WagerID != wager.ID() {
		t.Errorf("WagerID = %s, want %s", quote.WagerID, wager.ID())
	}
	// $10 at 2.00 with a fair price of 2.00: the position is worth the stake,
	// less the 5% named haircut.
	if quote.FairValue != domain.Money(10_00) {
		t.Errorf("FairValue = %s, want %s", quote.FairValue, domain.Money(10_00))
	}
	if quote.Value != domain.Money(9_50) {
		t.Errorf("Value = %s, want %s", quote.Value, domain.Money(9_50))
	}
	if !quote.QuotedAt.Equal(testNow) {
		t.Errorf("QuotedAt = %s, want the service's single instant %s", quote.QuotedAt, testNow)
	}
}

func TestServiceCashOutQuoteRefusals(t *testing.T) {
	t.Parallel()

	t.Run("a stale reference price", func(t *testing.T) {
		t.Parallel()
		// CLAUDE.md §9 makes odds staleness the headline SLO, and pricing an
		// early close off a minute-old line during a scoring drive is the
		// single most expensive moment to be wrong.
		svc, wager, _ := cashOutFixture(t, testNow.Add(-DefaultMaxFairPriceAge-time.Second))
		if _, err := svc.CashOutQuote(context.Background(), wager.ID()); !errors.Is(err, ErrCashOutUnavailable) {
			t.Fatalf("CashOutQuote() = %v, want ErrCashOutUnavailable", err)
		}
	})

	t.Run("a missing reference price", func(t *testing.T) {
		t.Parallel()
		svc, wager, fair := cashOutFixture(t, testNow)
		fair.prices = map[domain.SelectionID]FairPrice{}
		if _, err := svc.CashOutQuote(context.Background(), wager.ID()); !errors.Is(err, ErrCashOutUnavailable) {
			t.Fatalf("CashOutQuote() = %v, want ErrCashOutUnavailable", err)
		}
	})

	t.Run("an already terminal wager", func(t *testing.T) {
		t.Parallel()
		svc, wager, _ := cashOutFixture(t, testNow)

		graded, err := wager.GradeLeg(wager.Legs()[0].ID(), domain.LegStatusWon, testNow)
		if err != nil {
			t.Fatalf("GradeLeg: %v", err)
		}
		settled, err := graded.Settle(domain.WagerStatusWon, domain.Money(20_00), testNow)
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		svc.wagers = &fakeWagers{wagers: map[domain.WagerID]domain.Wager{settled.ID(): settled}}

		if _, err := svc.CashOutQuote(context.Background(), settled.ID()); !errors.Is(err, ErrCashOutUnavailable) {
			t.Fatalf("CashOutQuote() on a settled wager = %v, want ErrCashOutUnavailable", err)
		}
	})

	t.Run("an unknown wager", func(t *testing.T) {
		t.Parallel()
		svc, _, _ := cashOutFixture(t, testNow)
		if _, err := svc.CashOutQuote(context.Background(), "wgr-missing"); !errors.Is(err, ErrWagerNotFound) {
			t.Fatalf("CashOutQuote() = %v, want ErrWagerNotFound", err)
		}
	})

	t.Run("a service with no cash-out ports", func(t *testing.T) {
		t.Parallel()
		svc, _ := newTestService(t, newFakeTx())
		if _, err := svc.CashOutQuote(context.Background(), "wgr-1"); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("CashOutQuote() = %v, want ErrInvalidOptions", err)
		}
	})
}
