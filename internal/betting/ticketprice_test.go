package betting

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
)

// pricedLeg builds a leg on its own event, so a set of them is an independent
// parlay unless a test deliberately reuses an event id.
func pricedLeg(t *testing.T, n int, decimal float64, event domain.EventID) domain.Leg {
	t.Helper()
	sel := domain.SelectionID("sel-" + string(rune('a'+n)))
	if event == "" {
		event = domain.EventID("evt-" + string(rune('a'+n)))
	}
	leg, err := domain.NewLeg(domain.LegParams{
		ID:          domain.LegID("leg-" + string(rune('a'+n))),
		EventID:     event,
		MarketID:    domain.MarketID("mkt-" + string(rune('a'+n))),
		MarketType:  domain.MarketTypeMoneyline,
		Role:        domain.SelectionRoleHome,
		SelectionID: sel,
		Price:       mustPrice(t, sel, testBook, decimal, domain.NoLine(), testObserved),
	})
	if err != nil {
		t.Fatalf("NewLeg: %v", err)
	}
	return leg
}

func TestIndependentPricerTicketDecimal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ticket  func(*testing.T) Ticket
		want    float64
		wantErr error
	}{
		{
			name: "a straight is its leg's price",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindStraight, Legs: []domain.Leg{pricedLeg(t, 0, 2.5, "")}}
			},
			want: 2.5,
		},
		{
			name: "a two-leg parlay is the product",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindParlay, Legs: []domain.Leg{
					pricedLeg(t, 0, 2.0, ""), pricedLeg(t, 1, 1.5, ""),
				}}
			},
			want: 3.0,
		},
		{
			name: "a round robin combination is priced as the parlay it is",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindRoundRobin, Legs: []domain.Leg{
					pricedLeg(t, 0, 2.0, ""), pricedLeg(t, 1, 2.0, ""),
				}}
			},
			want: 4.0,
		},
		{
			// The refusal to misprice. Pricing correlated legs as independent
			// OVERPRICES the ticket, permanently, in the direction nobody
			// audits.
			name: "two legs on one event are refused",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindParlay, Legs: []domain.Leg{
					pricedLeg(t, 0, 2.0, "evt-shared"), pricedLeg(t, 1, 1.5, "evt-shared"),
				}}
			},
			wantErr: ErrSameGameUnsupported,
		},
		{
			// odds/parlay.go: deriving a teased price needs an empirical model
			// of how a sport's margins are distributed, and "inventing one here
			// would be fabricated data of exactly the kind the project
			// forbids".
			name: "a teaser is refused for want of a posted ladder",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindTeaser, TeaserPoints: 6, Legs: []domain.Leg{
					pricedLeg(t, 0, 1.91, ""), pricedLeg(t, 1, 1.91, ""),
				}}
			},
			wantErr: ErrTeaserUnsupported,
		},
		{
			name: "no legs is refused",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindStraight}
			},
			wantErr: ErrSlipEmpty,
		},
		{
			name: "a straight with two legs is refused",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindStraight, Legs: []domain.Leg{
					pricedLeg(t, 0, 2.0, ""), pricedLeg(t, 1, 1.5, ""),
				}}
			},
			wantErr: ErrLegCountForKind,
		},
		{
			name: "a parlay with one leg is refused",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Kind: domain.WagerKindParlay, Legs: []domain.Leg{pricedLeg(t, 0, 2.0, "")}}
			},
			wantErr: ErrLegCountForKind,
		},
		{
			name: "an unknown kind is refused",
			ticket: func(t *testing.T) Ticket {
				return Ticket{Legs: []domain.Leg{pricedLeg(t, 0, 2.0, "")}}
			},
			wantErr: domain.ErrUnknownWagerKind,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := IndependentPricer{}.TicketDecimal(context.Background(), tc.ticket(t))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("TicketDecimal() = (%g, %v), want errors.Is(_, %v)", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TicketDecimal() = %v", err)
			}
			if math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("TicketDecimal() = %g, want %g", got, tc.want)
			}
		})
	}
}

// TestIndependentPricerIsOrderIndependent pins the property odds.ParlayDecimal
// buys by multiplying in ascending price order: two customers who built the
// same parlay in a different order must be quoted the same number down to the
// last bit, or the ticket price becomes a function of the click sequence.
func TestIndependentPricerIsOrderIndependent(t *testing.T) {
	t.Parallel()

	legs := []domain.Leg{
		pricedLeg(t, 0, 1.37, ""),
		pricedLeg(t, 1, 4.10, ""),
		pricedLeg(t, 2, 2.05, ""),
		pricedLeg(t, 3, 9.50, ""),
	}
	reversed := make([]domain.Leg, len(legs))
	for i, leg := range legs {
		reversed[len(legs)-1-i] = leg
	}

	forward, err := IndependentPricer{}.TicketDecimal(context.Background(),
		Ticket{Kind: domain.WagerKindParlay, Legs: legs})
	if err != nil {
		t.Fatalf("TicketDecimal: %v", err)
	}
	backward, err := IndependentPricer{}.TicketDecimal(context.Background(),
		Ticket{Kind: domain.WagerKindParlay, Legs: reversed})
	if err != nil {
		t.Fatalf("TicketDecimal: %v", err)
	}
	if forward != backward {
		t.Fatalf("reordering the slip moved the quote: %v != %v", forward, backward)
	}
}

// TestIndependentPricerLongParlayStaysInTheWagerRange is the reason the ticket
// bound is domain.MaxWagerDecimal and not MaxDecimalOdds: a 20-leg parlay of
// even-money legs is 2^20, an ordinary ticket the market-price bound would
// wrongly reject (wager.go).
func TestIndependentPricerLongParlayStaysInTheWagerRange(t *testing.T) {
	t.Parallel()

	legs := make([]domain.Leg, 20)
	for i := range legs {
		legs[i] = pricedLeg(t, i, 2.0, "")
	}
	got, err := IndependentPricer{}.TicketDecimal(context.Background(),
		Ticket{Kind: domain.WagerKindParlay, Legs: legs})
	if err != nil {
		t.Fatalf("TicketDecimal() = %v", err)
	}
	if math.Abs(got-math.Pow(2, 20)) > 1e-6 {
		t.Fatalf("TicketDecimal() = %g, want 2^20", got)
	}
	if got <= domain.MaxDecimalOdds {
		t.Fatal("the fixture no longer exceeds MaxDecimalOdds and stops testing the bound it was built for")
	}
	if got > domain.MaxWagerDecimal {
		t.Fatalf("TicketDecimal() = %g, above domain.MaxWagerDecimal", got)
	}
}
