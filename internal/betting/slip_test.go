package betting

import (
	"errors"
	"math"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
)

func TestSlipValidate(t *testing.T) {
	t.Parallel()

	// base is a valid two-leg parlay; every case below breaks exactly one thing
	// about it, so the assertion is about that one thing and not about the
	// interaction of several.
	base := func() Slip {
		return Slip{
			Kind: domain.WagerKindParlay,
			Legs: []SlipLeg{
				{SelectionID: "sel-1", BookID: testBook, SeenDecimal: 2.0, SeenLine: domain.NoLine()},
				{SelectionID: "sel-2", BookID: testBook, SeenDecimal: 1.8, SeenLine: domain.NoLine()},
			},
			Stake:             domain.Money(1000),
			SeenTicketDecimal: 3.6,
			Rounding:          domain.RoundHalfAwayFromZero,
		}
	}

	tests := []struct {
		name  string
		slip  func() Slip
		want  error
		valid bool
	}{
		{name: "a valid parlay", slip: base, valid: true},
		{
			name: "a valid straight needs no ticket price",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindStraight
				s.Legs = s.Legs[:1]
				s.SeenTicketDecimal = 0
				return s
			},
			valid: true,
		},
		{
			name: "a valid round robin",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindRoundRobin
				s.Legs = append(s.Legs, SlipLeg{SelectionID: "sel-3", BookID: testBook, SeenDecimal: 2.5, SeenLine: domain.NoLine()})
				s.Sizes = []int{2}
				s.SeenTicketDecimal = 0
				return s
			},
			valid: true,
		},
		{
			name: "no legs",
			slip: func() Slip { s := base(); s.Legs = nil; return s },
			want: ErrSlipEmpty,
		},
		{
			name: "more legs than a ticket may carry",
			slip: func() Slip {
				s := base()
				s.Legs = make([]SlipLeg, domain.MaxWagerLegs+1)
				for i := range s.Legs {
					s.Legs[i] = SlipLeg{
						SelectionID: domain.SelectionID(string(rune('a' + i))),
						BookID:      testBook,
						SeenDecimal: 2.0,
						SeenLine:    domain.NoLine(),
					}
				}
				return s
			},
			want: ErrTooManyLegs,
		},
		{
			name: "the same selection twice",
			slip: func() Slip { s := base(); s.Legs[1].SelectionID = s.Legs[0].SelectionID; return s },
			want: ErrDuplicateSelection,
		},
		{
			name: "a zero stake",
			slip: func() Slip { s := base(); s.Stake = 0; return s },
			want: ErrStakeNotPositive,
		},
		{
			name: "a negative stake",
			slip: func() Slip { s := base(); s.Stake = -1; return s },
			want: ErrStakeNotPositive,
		},
		{
			name: "no rounding mode",
			slip: func() Slip { s := base(); s.Rounding = domain.RoundingUnknown; return s },
			want: domain.ErrUnknownRounding,
		},
		{
			name: "a straight with two legs",
			slip: func() Slip { s := base(); s.Kind = domain.WagerKindStraight; return s },
			want: ErrLegCountForKind,
		},
		{
			name: "a parlay with one leg",
			slip: func() Slip { s := base(); s.Legs = s.Legs[:1]; return s },
			want: ErrLegCountForKind,
		},
		{
			name: "a round robin over more selections than the exponential bound admits",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindRoundRobin
				s.SeenTicketDecimal = 0
				s.Sizes = []int{2}
				s.Legs = make([]SlipLeg, domain.MaxRoundRobinLegs+1)
				for i := range s.Legs {
					s.Legs[i] = SlipLeg{
						SelectionID: domain.SelectionID(string(rune('a' + i))),
						BookID:      testBook,
						SeenDecimal: 2.0,
						SeenLine:    domain.NoLine(),
					}
				}
				return s
			},
			want: ErrLegCountForKind,
		},
		{
			name: "a parlay carrying teaser points",
			slip: func() Slip { s := base(); s.TeaserPoints = 6; return s },
			want: ErrTeaserPoints,
		},
		{
			name: "a teaser carrying no points",
			slip: func() Slip { s := base(); s.Kind = domain.WagerKindTeaser; return s },
			want: ErrTeaserPoints,
		},
		{
			name: "a teaser past the points ceiling",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindTeaser
				s.TeaserPoints = domain.MaxTeaserPoints + 0.5
				return s
			},
			want: ErrTeaserPoints,
		},
		{
			name: "a teaser leg quoted with no line",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindTeaser
				s.TeaserPoints = 6
				return s
			},
			want: ErrTeaserMarketType,
		},
		{
			name: "a round robin naming no sizes",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindRoundRobin
				s.SeenTicketDecimal = 0
				return s
			},
			want: ErrRoundRobinSizes,
		},
		{
			name: "a round robin size larger than the selection count",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindRoundRobin
				s.SeenTicketDecimal = 0
				s.Sizes = []int{3}
				return s
			},
			want: ErrRoundRobinSizes,
		},
		{
			name: "a parlay naming combination sizes",
			slip: func() Slip { s := base(); s.Sizes = []int{2}; return s },
			want: ErrRoundRobinSizes,
		},
		{
			name: "a parlay stating no ticket price",
			slip: func() Slip { s := base(); s.SeenTicketDecimal = 0; return s },
			want: ErrInvalidSlip,
		},
		{
			name: "a round robin stating a ticket price it cannot have",
			slip: func() Slip {
				s := base()
				s.Kind = domain.WagerKindRoundRobin
				s.Sizes = []int{2}
				return s
			},
			want: ErrInvalidSlip,
		},
		{
			// A NaN seen price would compare unequal to every real quote and
			// turn every placement into a price-move refusal. See
			// Slip.validateSeenPrices.
			name: "a NaN seen price",
			slip: func() Slip { s := base(); s.Legs[0].SeenDecimal = math.NaN(); return s },
			want: domain.ErrOddsNotFinite,
		},
		{
			name: "a seen price below the domain floor",
			slip: func() Slip { s := base(); s.Legs[0].SeenDecimal = 1.0; return s },
			want: domain.ErrOddsOutOfRange,
		},
		{
			name: "a seen price above the market ceiling",
			slip: func() Slip { s := base(); s.Legs[0].SeenDecimal = domain.MaxDecimalOdds + 1; return s },
			want: domain.ErrOddsOutOfRange,
		},
		{
			name: "an accepted price that is not a price",
			slip: func() Slip {
				s := base()
				s.Legs[0].Accept = &Acceptance{Decimal: math.Inf(1), Line: domain.NoLine()}
				return s
			},
			want: domain.ErrOddsNotFinite,
		},
		{
			name: "a leg with no selection id",
			slip: func() Slip { s := base(); s.Legs[0].SelectionID = ""; return s },
			want: ErrInvalidSlip,
		},
		{
			name: "a leg naming no book",
			slip: func() Slip { s := base(); s.Legs[0].BookID = ""; return s },
			want: ErrInvalidSlip,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.slip().Validate()
			if tc.valid {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want %v", tc.want)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want errors.Is(_, %v)", err, tc.want)
			}
			// Every slip-shape refusal is in the ErrInvalidSlip class, which is
			// what an HTTP layer branches on. Asserted separately from the
			// specific sentinel so that a sentinel wrapping the wrong class is
			// a failure rather than a silent 500.
			if !errors.Is(err, ErrInvalidSlip) && !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("Validate() = %v, want it in the ErrInvalidSlip class", err)
			}
		})
	}
}

func TestSlipTicketCountAndTotalStake(t *testing.T) {
	t.Parallel()

	legs := func(n int) []SlipLeg {
		out := make([]SlipLeg, n)
		for i := range out {
			out[i] = SlipLeg{
				SelectionID: domain.SelectionID(string(rune('a' + i))),
				BookID:      testBook,
				SeenDecimal: 2.0,
				SeenLine:    domain.NoLine(),
			}
		}
		return out
	}

	tests := []struct {
		name       string
		slip       Slip
		wantTicket int
		wantTotal  domain.Money
	}{
		{
			name:       "a straight is one ticket",
			slip:       Slip{Kind: domain.WagerKindStraight, Legs: legs(1), Stake: 500},
			wantTicket: 1,
			wantTotal:  500,
		},
		{
			name:       "a parlay is one ticket however many legs",
			slip:       Slip{Kind: domain.WagerKindParlay, Legs: legs(4), Stake: 500},
			wantTicket: 1,
			wantTotal:  500,
		},
		{
			// wager.go's own example: "'$5 round robin by 2s' on four
			// selections risks $30, not $5" — C(4,2) = 6 tickets at $5.
			name:       "a four-selection round robin by 2s is six tickets",
			slip:       Slip{Kind: domain.WagerKindRoundRobin, Legs: legs(4), Sizes: []int{2}, Stake: 500},
			wantTicket: 6,
			wantTotal:  3000,
		},
		{
			name:       "by 2s and 3s adds the coefficients",
			slip:       Slip{Kind: domain.WagerKindRoundRobin, Legs: legs(4), Sizes: []int{2, 3}, Stake: 500},
			wantTicket: 10, // C(4,2)=6 + C(4,3)=4
			wantTotal:  5000,
		},
		{
			name:       "duplicate sizes describe the same round robin",
			slip:       Slip{Kind: domain.WagerKindRoundRobin, Legs: legs(4), Sizes: []int{3, 2, 3}, Stake: 500},
			wantTicket: 10,
			wantTotal:  5000,
		},
		{
			// The bound wager.go calls load-bearing: every size together over
			// ten selections is 2^10 − 11 = 1013 tickets.
			name: "every size over the maximum selections is 1013 tickets",
			slip: Slip{
				Kind:  domain.WagerKindRoundRobin,
				Legs:  legs(domain.MaxRoundRobinLegs),
				Sizes: []int{2, 3, 4, 5, 6, 7, 8, 9, 10},
				Stake: 100,
			},
			wantTicket: 1013,
			wantTotal:  101300,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.slip.TicketCount()
			if err != nil {
				t.Fatalf("TicketCount() = %v", err)
			}
			if got != tc.wantTicket {
				t.Fatalf("TicketCount() = %d, want %d", got, tc.wantTicket)
			}
			total, err := tc.slip.TotalStake()
			if err != nil {
				t.Fatalf("TotalStake() = %v", err)
			}
			if total != tc.wantTotal {
				t.Fatalf("TotalStake() = %s, want %s", total, tc.wantTotal)
			}
		})
	}
}

func TestSlipTotalStakeRefusesOverflow(t *testing.T) {
	t.Parallel()

	// A stake near the safe ceiling multiplied by six tickets overflows
	// domain.MaxSafeMoney, and TotalStake must say so rather than wrap. If it
	// wrapped, the balance check would compare a small positive number and the
	// customer would be booked for a bet nobody can pay.
	legs := make([]SlipLeg, 4)
	for i := range legs {
		legs[i] = SlipLeg{
			SelectionID: domain.SelectionID(string(rune('a' + i))),
			BookID:      testBook,
			SeenDecimal: 2.0,
			SeenLine:    domain.NoLine(),
		}
	}
	slip := Slip{
		Kind:     domain.WagerKindRoundRobin,
		Legs:     legs,
		Sizes:    []int{2},
		Stake:    domain.MaxSafeMoney,
		Rounding: domain.RoundHalfAwayFromZero,
	}
	if _, err := slip.TotalStake(); err == nil {
		t.Fatal("TotalStake() = nil error, want an overflow refusal")
	}
	if err := slip.Validate(); !errors.Is(err, ErrInvalidSlip) {
		t.Fatalf("Validate() = %v, want ErrInvalidSlip", err)
	}
}

// TestCheckPriceMove is the test for the invariant doc.go opens with: a ticket
// is never booked at a number the customer did not see and agree to.
func TestCheckPriceMove(t *testing.T) {
	t.Parallel()

	const sel = domain.SelectionID("sel-1")

	tests := []struct {
		name string

		seenDecimal float64
		seenLine    domain.Line
		accept      *Acceptance

		currentDecimal float64
		currentLine    domain.Line

		acceptBetter bool

		wantBooked   bool
		wantAccepted bool
		wantImproved bool
	}{
		{
			name:        "an unchanged quote books",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.91, currentLine: domain.NoLine(),
			wantBooked: true,
		},
		{
			name:        "a quote inside the match tolerance books",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.91 * (1 + priceMatchTolerance/10), currentLine: domain.NoLine(),
			wantBooked: true,
		},
		{
			name:        "a shorter price is refused",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.87, currentLine: domain.NoLine(),
			wantBooked: false,
		},
		{
			// The deliberate departure from what every real book does. See
			// slip.go's header: "accept when the new price is longer" and
			// "accept when the new price is shorter" are one operator apart.
			name:        "a LONGER price is refused too, without the opt-in",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.95, currentLine: domain.NoLine(),
			wantBooked: false, wantImproved: true,
		},
		{
			name:        "a longer price books with the opt-in",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.95, currentLine: domain.NoLine(),
			acceptBetter: true,
			wantBooked:   true,
		},
		{
			name:        "the opt-in never covers a shorter price",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			currentDecimal: 1.87, currentLine: domain.NoLine(),
			acceptBetter: true,
			wantBooked:   false,
		},
		{
			name:        "an explicit acceptance of the current price books",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			accept:         &Acceptance{Decimal: 1.87, Line: domain.NoLine()},
			currentDecimal: 1.87, currentLine: domain.NoLine(),
			wantBooked: true,
		},
		{
			name:        "an acceptance of a price that has itself moved is refused",
			seenDecimal: 1.91, seenLine: domain.NoLine(),
			accept:         &Acceptance{Decimal: 1.87, Line: domain.NoLine()},
			currentDecimal: 1.83, currentLine: domain.NoLine(),
			wantBooked: false, wantAccepted: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg := SlipLeg{
				SelectionID: sel,
				BookID:      testBook,
				SeenDecimal: tc.seenDecimal,
				SeenLine:    tc.seenLine,
				Accept:      tc.accept,
			}
			current := mustPrice(t, sel, testBook, tc.currentDecimal, tc.currentLine, testObserved)

			move := checkPriceMove(leg, current, tc.acceptBetter)
			if tc.wantBooked {
				if move != nil {
					t.Fatalf("checkPriceMove() = %v, want nil (bookable)", move)
				}
				return
			}
			if move == nil {
				t.Fatal("checkPriceMove() = nil, want a refusal")
			}
			if move.Accepted != tc.wantAccepted {
				t.Errorf("PriceMove.Accepted = %v, want %v", move.Accepted, tc.wantAccepted)
			}
			if move.Improved != tc.wantImproved {
				t.Errorf("PriceMove.Improved = %v, want %v", move.Improved, tc.wantImproved)
			}
			// Both spellings must satisfy the one sentinel a caller branches on.
			if !errors.Is(move, ErrPriceMoved) {
				t.Errorf("PriceMove = %v, want errors.Is(_, ErrPriceMoved)", move)
			}
			if tc.wantAccepted && !errors.Is(move, ErrPriceMovedNotAccepted) {
				t.Errorf("PriceMove = %v, want errors.Is(_, ErrPriceMovedNotAccepted)", move)
			}
		})
	}
}

// TestCheckPriceMoveOnALineMove is separate because the rule is different in
// kind: no amount of "better" waves a line move through. See slip.go's header.
func TestCheckPriceMoveOnALineMove(t *testing.T) {
	t.Parallel()

	const sel = domain.SelectionID("sel-1")
	seen := SlipLeg{SelectionID: sel, BookID: testBook, SeenDecimal: 1.91, SeenLine: mustLine(t, -3.5)}

	tests := []struct {
		name         string
		current      domain.Price
		acceptBetter bool
		accept       *Acceptance
		wantBooked   bool
	}{
		{
			name:       "the same price at a different line is refused",
			current:    mustPrice(t, sel, testBook, 1.91, mustLine(t, -4), testObserved),
			wantBooked: false,
		},
		{
			name:         "a longer price at a different line is refused even with the opt-in",
			current:      mustPrice(t, sel, testBook, 1.95, mustLine(t, -4), testObserved),
			acceptBetter: true,
			wantBooked:   false,
		},
		{
			name:       "an acceptance naming the price but not the line is refused",
			current:    mustPrice(t, sel, testBook, 1.91, mustLine(t, -4), testObserved),
			accept:     &Acceptance{Decimal: 1.91, Line: mustLine(t, -3.5)},
			wantBooked: false,
		},
		{
			name:       "an acceptance naming both books",
			current:    mustPrice(t, sel, testBook, 1.91, mustLine(t, -4), testObserved),
			accept:     &Acceptance{Decimal: 1.91, Line: mustLine(t, -4)},
			wantBooked: true,
		},
		{
			// A present 0.0 is a traded pick'em and is a different fact from
			// "no line" — which is why SeenLine is a domain.Line and not a
			// *float64.
			name:       "a pick'em is not the same as no line",
			current:    mustPrice(t, sel, testBook, 1.91, domain.NoLine(), testObserved),
			wantBooked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			leg := seen
			leg.Accept = tc.accept
			move := checkPriceMove(leg, tc.current, tc.acceptBetter)
			if tc.wantBooked != (move == nil) {
				t.Fatalf("checkPriceMove() = %v, wantBooked = %v", move, tc.wantBooked)
			}
			if move != nil && move.Improved {
				t.Errorf("PriceMove.Improved = true across a line move; \"better\" has no meaning there")
			}
		})
	}
}

func TestCheckTicketPriceMove(t *testing.T) {
	t.Parallel()

	accept := func(v float64) *float64 { return &v }

	tests := []struct {
		name       string
		slip       Slip
		current    float64
		wantBooked bool
	}{
		{
			name:       "no stated ticket price is not checked",
			slip:       Slip{},
			current:    3.6,
			wantBooked: true,
		},
		{
			name:       "an unchanged ticket price books",
			slip:       Slip{SeenTicketDecimal: 3.6},
			current:    3.6,
			wantBooked: true,
		},
		{
			name:       "a moved ticket price is refused",
			slip:       Slip{SeenTicketDecimal: 3.6},
			current:    3.4,
			wantBooked: false,
		},
		{
			name:       "an improved ticket price is refused without the opt-in",
			slip:       Slip{SeenTicketDecimal: 3.6},
			current:    3.8,
			wantBooked: false,
		},
		{
			name:       "an improved ticket price books with the opt-in",
			slip:       Slip{SeenTicketDecimal: 3.6, AcceptBetterPrice: true},
			current:    3.8,
			wantBooked: true,
		},
		{
			name:       "an explicit ticket acceptance books",
			slip:       Slip{SeenTicketDecimal: 3.6, AcceptTicketDecimal: accept(3.4)},
			current:    3.4,
			wantBooked: true,
		},
		{
			name:       "a ticket acceptance that has itself moved is refused",
			slip:       Slip{SeenTicketDecimal: 3.6, AcceptTicketDecimal: accept(3.4)},
			current:    3.2,
			wantBooked: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			move := checkTicketPriceMove(tc.slip, tc.current)
			if tc.wantBooked != (move == nil) {
				t.Fatalf("checkTicketPriceMove() = %v, wantBooked = %v", move, tc.wantBooked)
			}
		})
	}
}

// TestTeasedLine asserts the DIRECTION rule migrations/00006 installed a
// database check for after finding domain.validateTeaser could not see a sign
// error. A wrong-direction tease has the right magnitude, so the magnitude test
// passes and the customer grades at a handicap nobody sold.
func TestTeasedLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		role   domain.SelectionRole
		booked float64
		points float64
		want   float64
	}{
		{name: "a home favourite gets points added", role: domain.SelectionRoleHome, booked: -3.5, points: 6, want: 2.5},
		{name: "a home underdog gets points added", role: domain.SelectionRoleHome, booked: 3.5, points: 6, want: 9.5},
		{
			// price_line is already stated from the selection's own
			// perspective, so an away spread needs no special case here.
			name: "an away side gets points added on its own inverted line",
			role: domain.SelectionRoleAway, booked: 3.5, points: 6, want: 9.5,
		},
		{name: "an over's threshold moves DOWN", role: domain.SelectionRoleOver, booked: 47.5, points: 6, want: 41.5},
		{name: "an under's threshold moves UP", role: domain.SelectionRoleUnder, booked: 47.5, points: 6, want: 53.5},
		{
			// legs.teased_line is deliberately not constrained positive, unlike
			// markets.line: a heavily teased low total may legitimately cross
			// zero.
			name: "a heavily teased low total may cross zero",
			role: domain.SelectionRoleOver, booked: 4.5, points: 6, want: -1.5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := teasedLine(tc.role, mustLine(t, tc.booked), tc.points)
			if err != nil {
				t.Fatalf("teasedLine() = %v", err)
			}
			v, ok := got.Value()
			if !ok {
				t.Fatal("teasedLine() returned no line")
			}
			// Lines are quarter-point multiples and teaser points half-point
			// multiples, all dyadic and therefore exact in float64, so a
			// correct tease differs from the promise by exactly zero.
			if v != tc.want {
				t.Fatalf("teasedLine() = %g, want %g", v, tc.want)
			}
			// The check migration 00006 installed: the magnitude is right AND
			// the sign is right.
			if math.Abs(math.Abs(v-tc.booked)-tc.points) > 1e-9 {
				t.Fatalf("teased %g from %g is not %g points", v, tc.booked, tc.points)
			}
		})
	}
}

func TestTeasedLineRefusesAMarketWithNoLine(t *testing.T) {
	t.Parallel()
	if _, err := teasedLine(domain.SelectionRoleHome, domain.NoLine(), 6); !errors.Is(err, ErrTeaserMarketType) {
		t.Fatalf("teasedLine(NoLine) = %v, want ErrTeaserMarketType", err)
	}
}

func TestFloatsMatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b float64
		want bool
	}{
		{name: "identical", a: 1.91, b: 1.91, want: true},
		{name: "one tick apart at even money", a: 1.91, b: 1.92, want: false},
		{name: "one tick apart on a long price", a: 501.0, b: 501.5, want: false},
		{name: "inside the tolerance", a: 1.91, b: 1.91 * (1 + priceMatchTolerance/2), want: true},
		{name: "just outside the tolerance", a: 1.91, b: 1.91 * (1 + 10*priceMatchTolerance), want: false},
		{name: "NaN never matches itself", a: math.NaN(), b: math.NaN(), want: false},
		{name: "NaN never matches a price", a: math.NaN(), b: 1.91, want: false},
		{name: "both zero", a: 0, b: 0, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := floatsMatch(tc.a, tc.b); got != tc.want {
				t.Fatalf("floatsMatch(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
