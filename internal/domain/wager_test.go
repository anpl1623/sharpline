package domain

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// straightParams is a $100 straight on the canonical -110 spread. Every
// validation test mutates a copy of it, so exactly one field differs from a
// known-good ticket in each case.
func straightParams(t *testing.T) WagerParams {
	t.Helper()
	leg := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	return WagerParams{
		ID:              "wgr-1",
		UserID:          "usr-1",
		Kind:            WagerKindStraight,
		Legs:            []Leg{leg},
		Stake:           mustMajorUnits(t, 100),
		AcceptedDecimal: priceMinus110,
		Rounding:        RoundHalfAwayFromZero,
		PlacedAt:        ts(time.Minute),
	}
}

// mustMajorUnits builds a Money from whole major units or fails the test.
func mustMajorUnits(t *testing.T, n int64) Money {
	t.Helper()
	m, err := FromMajorUnits(n)
	if err != nil {
		t.Fatalf("FromMajorUnits(%d): %v", n, err)
	}
	return m
}

// mustMoney parses a decimal amount in major units or fails the test.
func mustMoney(t *testing.T, s string) Money {
	t.Helper()
	m, err := ParseMoney(s)
	if err != nil {
		t.Fatalf("ParseMoney(%q): %v", s, err)
	}
	return m
}

// mustWager builds a wager or fails the test.
func mustWager(t *testing.T, p WagerParams) Wager {
	t.Helper()
	w, err := NewWager(p)
	if err != nil {
		t.Fatalf("NewWager(%s): %v", p.ID, err)
	}
	return w
}

// distinctLegs builds n legs, each on its own event and market, so no
// duplicate-selection or duplicate-market rule fires.
func distinctLegs(t *testing.T, n int) []Leg {
	t.Helper()
	legs := make([]Leg, 0, n)
	for i := range n {
		s := strconv.Itoa(i)
		legs = append(legs, mustLeg(t, moneylineSpec("leg-"+s, "evt-"+s, "mkt-"+s, pricePlus150)))
	}
	return legs
}

// binomial computes C(n, k) by an independent route from combinationIndexes,
// so the two can disagree.
func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	result := 1
	for i := 1; i <= k; i++ {
		result = result * (n - k + i) / i
	}
	return result
}

// ---------------------------------------------------------------------------
// Enums
// ---------------------------------------------------------------------------

func TestWagerKindTextRoundTrip(t *testing.T) {
	cases := []struct {
		kind WagerKind
		text string
	}{
		{WagerKindStraight, "straight"},
		{WagerKindParlay, "parlay"},
		{WagerKindRoundRobin, "round_robin"},
		{WagerKindTeaser, "teaser"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := c.kind.String(); got != c.text {
				t.Errorf("String() = %q, want %q", got, c.text)
			}
			parsed, err := ParseWagerKind(c.text)
			if err != nil {
				t.Fatalf("ParseWagerKind: %v", err)
			}
			if parsed != c.kind {
				t.Errorf("ParseWagerKind(%q) = %v", c.text, parsed)
			}
			b, err := c.kind.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var back WagerKind
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText: %v", err)
			}
			if back != c.kind {
				t.Errorf("round trip = %v, want %v", back, c.kind)
			}
		})
	}
	if _, err := ParseWagerKind("roundrobin"); !errors.Is(err, ErrUnknownWagerKind) {
		t.Errorf("ParseWagerKind of a near-miss spelling: %v", err)
	}
	if _, err := WagerKindUnknown.MarshalText(); !errors.Is(err, ErrUnknownWagerKind) {
		t.Errorf("MarshalText of the zero value: %v", err)
	}
	if WagerKindStraight.IsMulti() {
		t.Error("a straight reports as multi-leg")
	}
	for _, k := range []WagerKind{WagerKindParlay, WagerKindRoundRobin, WagerKindTeaser} {
		if !k.IsMulti() {
			t.Errorf("%v does not report as multi-leg", k)
		}
	}
}

func TestWagerStatusTextRoundTrip(t *testing.T) {
	cases := []struct {
		status WagerStatus
		text   string
	}{
		{WagerStatusPlaced, "placed"},
		{WagerStatusOpen, "open"},
		{WagerStatusWon, "won"},
		{WagerStatusLost, "lost"},
		{WagerStatusVoid, "void"},
		{WagerStatusPush, "push"},
		{WagerStatusCashedOut, "cashed_out"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := c.status.String(); got != c.text {
				t.Errorf("String() = %q, want %q", got, c.text)
			}
			parsed, err := ParseWagerStatus(c.text)
			if err != nil {
				t.Fatalf("ParseWagerStatus: %v", err)
			}
			if parsed != c.status {
				t.Errorf("ParseWagerStatus(%q) = %v", c.text, parsed)
			}
			b, err := c.status.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var back WagerStatus
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText: %v", err)
			}
			if back != c.status {
				t.Errorf("round trip = %v", back)
			}
		})
	}
	if _, err := ParseWagerStatus("pending"); !errors.Is(err, ErrUnknownWagerStatus) {
		t.Errorf("ParseWagerStatus of a leg-only status: %v", err)
	}
	if _, err := WagerStatusUnknown.MarshalText(); !errors.Is(err, ErrUnknownWagerStatus) {
		t.Errorf("MarshalText of the zero value: %v", err)
	}
}

// TestWagerStatusPredicates pins the three classifications the settlement path
// and the exposure metric branch on.
func TestWagerStatusPredicates(t *testing.T) {
	cases := []struct {
		status      WagerStatus
		terminal    bool
		graded      bool
		holdsEscrow bool
	}{
		{WagerStatusPlaced, false, false, true},
		{WagerStatusOpen, false, false, true},
		{WagerStatusWon, true, true, false},
		{WagerStatusLost, true, true, false},
		{WagerStatusVoid, true, true, false},
		{WagerStatusPush, true, true, false},
		{WagerStatusCashedOut, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.status.String(), func(t *testing.T) {
			if got := c.status.IsTerminal(); got != c.terminal {
				t.Errorf("IsTerminal() = %v, want %v", got, c.terminal)
			}
			if got := c.status.IsGraded(); got != c.graded {
				t.Errorf("IsGraded() = %v, want %v", got, c.graded)
			}
			if got := c.status.HoldsEscrow(); got != c.holdsEscrow {
				t.Errorf("HoldsEscrow() = %v, want %v", got, c.holdsEscrow)
			}
		})
	}
}

// TestWagerStatusTransitionMatrix asserts every ordered pair against an
// explicit table of legal edges.
func TestWagerStatusTransitionMatrix(t *testing.T) {
	all := []WagerStatus{
		WagerStatusUnknown, WagerStatusPlaced, WagerStatusOpen, WagerStatusWon,
		WagerStatusLost, WagerStatusVoid, WagerStatusPush, WagerStatusCashedOut,
	}
	allowed := map[WagerStatus][]WagerStatus{
		WagerStatusPlaced: {
			WagerStatusPlaced, WagerStatusOpen, WagerStatusWon, WagerStatusLost,
			WagerStatusVoid, WagerStatusPush, WagerStatusCashedOut,
		},
		WagerStatusOpen: {
			WagerStatusOpen, WagerStatusWon, WagerStatusLost,
			WagerStatusVoid, WagerStatusPush, WagerStatusCashedOut,
		},
		WagerStatusWon:       {WagerStatusWon},
		WagerStatusLost:      {WagerStatusLost},
		WagerStatusVoid:      {WagerStatusVoid},
		WagerStatusPush:      {WagerStatusPush},
		WagerStatusCashedOut: {WagerStatusCashedOut},
	}
	for _, from := range all {
		for _, to := range all {
			want := slices.Contains(allowed[from], to)
			if got := from.CanTransitionTo(to); got != want {
				t.Errorf("%v.CanTransitionTo(%v) = %v, want %v", from, to, got, want)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewWagerRejectsBadInput(t *testing.T) {
	spread := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	total := mustLeg(t, totalSpec(t, "leg-2", "evt-1", "mkt-2"))

	cases := []struct {
		name   string
		mutate func(t *testing.T, p *WagerParams)
		want   error
	}{
		{"empty wager id", func(_ *testing.T, p *WagerParams) { p.ID = "" }, ErrEmptyID},
		{"empty user id", func(_ *testing.T, p *WagerParams) { p.UserID = "" }, ErrEmptyID},
		{"unset kind", func(_ *testing.T, p *WagerParams) { p.Kind = WagerKindUnknown }, ErrUnknownWagerKind},
		{"no legs", func(_ *testing.T, p *WagerParams) { p.Legs = nil }, ErrLegCount},
		{"a straight with two legs", func(_ *testing.T, p *WagerParams) {
			p.Legs = []Leg{spread, total}
		}, ErrLegCount},
		{"a parlay with one leg", func(_ *testing.T, p *WagerParams) {
			p.Kind = WagerKindParlay
		}, ErrLegCount},
		{"past the leg cap", func(t *testing.T, p *WagerParams) {
			p.Kind = WagerKindParlay
			p.Legs = distinctLegs(t, MaxWagerLegs+1)
			p.AcceptedDecimal = 2
		}, ErrLegCount},
		{"the same selection twice", func(_ *testing.T, p *WagerParams) {
			p.Kind = WagerKindParlay
			p.Legs = []Leg{spread, spread}
			p.AcceptedDecimal = 2
		}, ErrDuplicateSelection},
		{"two answers to one market", func(t *testing.T, p *WagerParams) {
			other := spreadSpec(t, "leg-away", "evt-1", "mkt-1")
			other.role = SelectionRoleAway
			p.Kind = WagerKindParlay
			p.Legs = []Leg{spread, mustLeg(t, other)}
			p.AcceptedDecimal = 2
		}, ErrDuplicateMarket},
		{"an already graded leg", func(t *testing.T, p *WagerParams) {
			graded, err := spread.WithStatus(LegStatusWon, ts(time.Hour))
			if err != nil {
				t.Fatalf("WithStatus: %v", err)
			}
			p.Legs = []Leg{graded}
		}, ErrLegNotPending},
		{"a zero stake", func(_ *testing.T, p *WagerParams) { p.Stake = ZeroMoney }, ErrStakeNotPositive},
		{"a negative stake", func(t *testing.T, p *WagerParams) {
			p.Stake = mustMoney(t, "-10.00")
		}, ErrStakeNotPositive},
		{"a price of exactly 1.0", func(_ *testing.T, p *WagerParams) {
			p.AcceptedDecimal = MinDecimalOdds
		}, ErrWagerOddsOutOfRange},
		{"a NaN price", func(_ *testing.T, p *WagerParams) {
			p.AcceptedDecimal = math.NaN()
		}, ErrOddsNotFinite},
		{"an infinite price", func(_ *testing.T, p *WagerParams) {
			p.AcceptedDecimal = math.Inf(1)
		}, ErrOddsNotFinite},
		{"a price past the ticket cap", func(_ *testing.T, p *WagerParams) {
			p.Kind = WagerKindParlay
			p.Legs = []Leg{spread, total}
			p.AcceptedDecimal = MaxWagerDecimal * 2
		}, ErrWagerOddsOutOfRange},
		{"a straight priced away from its leg", func(_ *testing.T, p *WagerParams) {
			p.AcceptedDecimal = priceMinus105
		}, ErrWagerPriceMismatch},
		{"an unset rounding mode", func(_ *testing.T, p *WagerParams) {
			p.Rounding = RoundingUnknown
		}, ErrUnknownRounding},
		{"a zero placement time", func(_ *testing.T, p *WagerParams) {
			p.PlacedAt = time.Time{}
		}, ErrZeroTime},
		{"teaser points on a straight", func(_ *testing.T, p *WagerParams) {
			p.TeaserPoints = 6
		}, ErrTeaserPointsNotApplicable},
		{"a teased leg on a straight", func(t *testing.T, p *WagerParams) {
			teased, err := spread.WithTeasedLine(mustLine(t, 2.5))
			if err != nil {
				t.Fatalf("WithTeasedLine: %v", err)
			}
			p.Legs = []Leg{teased}
		}, ErrTeasedLegNotApplicable},
		{"a round robin parent on a straight", func(_ *testing.T, p *WagerParams) {
			p.RoundRobinID = "rr-1"
		}, ErrRoundRobinParentNotApplicable},
		{"a round robin ticket with no parent", func(_ *testing.T, p *WagerParams) {
			p.Kind = WagerKindRoundRobin
			p.Legs = []Leg{spread, total}
			p.AcceptedDecimal = 2
		}, ErrRoundRobinParentRequired},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := straightParams(t)
			c.mutate(t, &p)
			_, err := NewWager(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewWager: %v, want %v", err, c.want)
			}
			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
				t.Errorf("error %v reaches neither taxonomy root", err)
			}
		})
	}
}

// TestNewWagerIsAlwaysBornPlaced asserts the construction invariant the whole
// state machine rests on: there is no way to conjure a settled wager.
func TestNewWagerIsAlwaysBornPlaced(t *testing.T) {
	w := mustWager(t, straightParams(t))

	if w.Status() != WagerStatusPlaced {
		t.Errorf("status = %v, want placed", w.Status())
	}
	if _, settled := w.Returned(); settled {
		t.Error("a freshly placed wager reports a returned amount")
	}
	if _, ok := w.SettledAt(); ok {
		t.Error("a freshly placed wager reports a settlement time")
	}
	if !w.UpdatedAt().Equal(w.PlacedAt()) {
		t.Errorf("UpdatedAt %s does not seed from PlacedAt %s", w.UpdatedAt(), w.PlacedAt())
	}
	if w.IsZero() {
		t.Error("a constructed wager reports IsZero")
	}
	if !(Wager{}).IsZero() {
		t.Error("the zero Wager does not report IsZero")
	}
	if _, isRR := w.RoundRobinID(); isRR {
		t.Error("a straight claims a round robin parent")
	}
	if _, isTeaser := w.TeaserPoints(); isTeaser {
		t.Error("a straight claims teaser points")
	}
}

// TestWagerLegsAreCopied asserts the defensive copy: grading a leg obtained
// from Legs() must not reach back into the wager.
func TestWagerLegsAreCopied(t *testing.T) {
	w := mustWager(t, straightParams(t))

	legs := w.Legs()
	graded, err := legs[0].WithStatus(LegStatusWon, ts(time.Hour))
	if err != nil {
		t.Fatalf("WithStatus: %v", err)
	}
	legs[0] = graded

	if w.Legs()[0].Status() != LegStatusPending {
		t.Error("mutating the slice returned by Legs() reached the wager")
	}
	if w.LegCount() != 1 {
		t.Errorf("LegCount() = %d, want 1", w.LegCount())
	}
	if _, ok := w.Leg("leg-1"); !ok {
		t.Error("Leg() cannot find a leg that is on the ticket")
	}
	if _, ok := w.Leg("leg-absent"); ok {
		t.Error("Leg() found a leg that is not on the ticket")
	}
}

// ---------------------------------------------------------------------------
// Payout arithmetic
// ---------------------------------------------------------------------------

// TestStraightPayoutArithmetic checks the single multiplication this type does
// against results that are public knowledge in the domain: $100 at -110 wins
// $90.91, and a two-team -110 parlay pays +264, so $50 wins $132.23.
func TestStraightPayoutArithmetic(t *testing.T) {
	cases := []struct {
		name     string
		stake    string
		decimal  float64
		rounding Rounding
		payout   string
		profit   string
	}{
		{"100 at -110, commercial rounding", "100.00", priceMinus110, RoundHalfAwayFromZero, "190.91", "90.91"},
		{"100 at -110, truncated", "100.00", priceMinus110, RoundTowardZero, "190.90", "90.90"},
		{"100 at -110, banker's rounding", "100.00", priceMinus110, RoundHalfToEven, "190.91", "90.91"},
		{"100 at +150", "100.00", pricePlus150, RoundHalfAwayFromZero, "250.00", "150.00"},
		{"200 at -200", "200.00", priceMinus200, RoundHalfAwayFromZero, "300.00", "100.00"},
		{"1 at -110 rounds to a whole cent", "1.00", priceMinus110, RoundHalfAwayFromZero, "1.91", "0.91"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			leg := mustLeg(t, betLegSpec{
				legID: "leg-1", eventID: "evt-1", marketID: "mkt-1",
				typ: MarketTypeMoneyline, line: NoLine(),
				role: SelectionRoleHome, decimal: c.decimal,
			})
			w := mustWager(t, WagerParams{
				ID: "wgr-1", UserID: "usr-1", Kind: WagerKindStraight,
				Legs: []Leg{leg}, Stake: mustMoney(t, c.stake),
				AcceptedDecimal: c.decimal, Rounding: c.rounding, PlacedAt: ts(0),
			})
			if got, want := w.PotentialPayout(), mustMoney(t, c.payout); got.Compare(want) != 0 {
				t.Errorf("PotentialPayout() = %s, want %s", got, want)
			}
			if got, want := w.PotentialProfit(), mustMoney(t, c.profit); got.Compare(want) != 0 {
				t.Errorf("PotentialProfit() = %s, want %s", got, want)
			}
			if w.Rounding() != c.rounding {
				t.Errorf("Rounding() = %v, want %v", w.Rounding(), c.rounding)
			}
		})
	}
}

// TestParlayPriceIsStoredNotDerived is the argument for the accepted-price
// field: a correlated same-game parlay is priced BELOW the product of its legs,
// so re-deriving the price at settlement would pay a number the customer never
// accepted.
func TestParlayPriceIsStoredNotDerived(t *testing.T) {
	spread := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	total := mustLeg(t, totalSpec(t, "leg-2", "evt-1", "mkt-2"))

	product := spread.QuotedDecimal() * total.QuotedDecimal()
	// A two-team -110 parlay prices at +264 — decimal 3.6446 — when the legs
	// are independent.
	assertApprox(t, "independent two-leg -110 product", product, 3.644628099173554)

	// Same game, so the legs are correlated and the book quotes less.
	const correlated = 3.2
	w := mustWager(t, WagerParams{
		ID: "wgr-sgp", UserID: "usr-1", Kind: WagerKindParlay,
		Legs: []Leg{spread, total}, Stake: mustMoney(t, "50.00"),
		AcceptedDecimal: correlated, Rounding: RoundHalfAwayFromZero, PlacedAt: ts(0),
	})

	if !w.IsSameGame() {
		t.Error("two legs on one event do not report as a same-game parlay")
	}
	if ids := w.EventIDs(); len(ids) != 1 || ids[0] != "evt-1" {
		t.Errorf("EventIDs() = %v, want [evt-1]", ids)
	}
	assertApprox(t, "accepted price", w.AcceptedDecimal(), correlated)
	if got, want := w.PotentialPayout(), mustMoney(t, "160.00"); got.Compare(want) != 0 {
		t.Errorf("PotentialPayout() = %s, want %s", got, want)
	}

	// An independent two-leg parlay across two events is not same-game.
	other := mustLeg(t, totalSpec(t, "leg-3", "evt-2", "mkt-3"))
	across := mustWager(t, WagerParams{
		ID: "wgr-par", UserID: "usr-1", Kind: WagerKindParlay,
		Legs: []Leg{spread, other}, Stake: mustMoney(t, "50.00"),
		AcceptedDecimal: product, Rounding: RoundHalfAwayFromZero, PlacedAt: ts(0),
	})
	if across.IsSameGame() {
		t.Error("legs on two events report as a same-game parlay")
	}
	if ids := across.EventIDs(); len(ids) != 2 {
		t.Errorf("EventIDs() = %v, want two distinct events", ids)
	}
	if got, want := across.PotentialPayout(), mustMoney(t, "182.23"); got.Compare(want) != 0 {
		t.Errorf("PotentialPayout() = %s, want %s (a two-team -110 parlay pays +264)", got, want)
	}
}

// ---------------------------------------------------------------------------
// Teasers
// ---------------------------------------------------------------------------

// teaserWager builds the canonical 2-team 6-point football teaser: a -3.5
// spread teased to +2.5 and an over 47.5 teased to 41.5, priced at -110.
func teaserWager(t *testing.T, points float64, spreadTeased, totalTeased float64) (WagerParams, error) {
	t.Helper()
	spread := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	total := mustLeg(t, totalSpec(t, "leg-2", "evt-2", "mkt-2"))

	teasedSpread, err := spread.WithTeasedLine(mustLine(t, spreadTeased))
	if err != nil {
		return WagerParams{}, err
	}
	teasedTotal, err := total.WithTeasedLine(mustLine(t, totalTeased))
	if err != nil {
		return WagerParams{}, err
	}
	return WagerParams{
		ID: "wgr-teaser", UserID: "usr-1", Kind: WagerKindTeaser,
		Legs:            []Leg{teasedSpread, teasedTotal},
		Stake:           mustMoney(t, "20.00"),
		AcceptedDecimal: priceMinus110,
		Rounding:        RoundHalfAwayFromZero,
		TeaserPoints:    points,
		PlacedAt:        ts(0),
	}, nil
}

func TestTeaserValidation(t *testing.T) {
	t.Run("a correctly teased ticket is accepted", func(t *testing.T) {
		p, err := teaserWager(t, 6, 2.5, 41.5)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		w := mustWager(t, p)

		points, ok := w.TeaserPoints()
		if !ok {
			t.Fatal("a teaser does not report teaser points")
		}
		assertApprox(t, "teaser points", points, 6)
		// A 2-team 6-point teaser at -110 wins $18.18 on $20.
		if got, want := w.PotentialProfit(), mustMoney(t, "18.18"); got.Compare(want) != 0 {
			t.Errorf("PotentialProfit() = %s, want %s", got, want)
		}
		for _, leg := range w.Legs() {
			if !leg.TeasedLine().Present() {
				t.Errorf("leg %s lost its teased line", leg.ID())
			}
		}
	})

	t.Run("a leg teased by the wrong amount is refused", func(t *testing.T) {
		// 47.5 down to 42.5 is five points, not six.
		p, err := teaserWager(t, 6, 2.5, 42.5)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if _, err := NewWager(p); !errors.Is(err, ErrTeaserPointsMismatch) {
			t.Fatalf("NewWager: %v, want ErrTeaserPointsMismatch", err)
		}
	})

	t.Run("a leg teased in the wrong direction is still six points", func(t *testing.T) {
		// -3.5 moved to -9.5 is six points AGAINST the bettor. The magnitude
		// check accepts it; catching the direction needs the selection's role
		// and is a settlement-service rule. This test pins the known gap.
		p, err := teaserWager(t, 6, -9.5, 41.5)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if _, err := NewWager(p); err != nil {
			t.Fatalf("NewWager: %v", err)
		}
	})

	t.Run("an unteased leg is refused", func(t *testing.T) {
		p, err := teaserWager(t, 6, 2.5, 41.5)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		p.Legs = []Leg{
			mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1")),
			p.Legs[1],
		}
		if _, err := NewWager(p); !errors.Is(err, ErrTeasedLegRequired) {
			t.Fatalf("NewWager: %v, want ErrTeasedLegRequired", err)
		}
	})

	t.Run("teaser points outside the guard rails are refused", func(t *testing.T) {
		for _, points := range []float64{0, -6, MaxTeaserPoints + 1} {
			p, err := teaserWager(t, 6, 2.5, 41.5)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			p.TeaserPoints = points
			if _, err := NewWager(p); !errors.Is(err, ErrTeaserPointsRequired) {
				t.Errorf("NewWager with %v points: %v, want ErrTeaserPointsRequired", points, err)
			}
		}
	})

	t.Run("a one-leg teaser is refused", func(t *testing.T) {
		p, err := teaserWager(t, 6, 2.5, 41.5)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		p.Legs = p.Legs[:1]
		if _, err := NewWager(p); !errors.Is(err, ErrLegCount) {
			t.Fatalf("NewWager: %v, want ErrLegCount", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

func TestWagerLifecycle(t *testing.T) {
	t.Run("placed → open → won", func(t *testing.T) {
		w := mustWager(t, straightParams(t))

		open, err := w.Open(ts(2 * time.Hour))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if open.Status() != WagerStatusOpen {
			t.Errorf("status = %v, want open", open.Status())
		}
		if w.Status() != WagerStatusPlaced {
			t.Error("Open mutated the receiver")
		}

		settled, err := open.Settle(WagerStatusWon, open.PotentialPayout(), ts(5*time.Hour))
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		returned, ok := settled.Returned()
		if !ok {
			t.Fatal("a settled wager reports no returned amount")
		}
		if returned.Compare(mustMoney(t, "190.91")) != 0 {
			t.Errorf("Returned() = %s, want 190.91", returned)
		}
		net, _ := settled.NetReturn()
		if net.Compare(mustMoney(t, "90.91")) != 0 {
			t.Errorf("NetReturn() = %s, want 90.91", net)
		}
		at, ok := settled.SettledAt()
		if !ok || !at.Equal(ts(5*time.Hour)) {
			t.Errorf("SettledAt() = %s, %v", at, ok)
		}
	})

	t.Run("placed settles directly, without an open hop", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		voided, err := w.Settle(WagerStatusVoid, w.Stake(), ts(time.Hour))
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if voided.Status() != WagerStatusVoid {
			t.Errorf("status = %v, want void", voided.Status())
		}
	})

	t.Run("terminal is terminal", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		lost, err := w.Settle(WagerStatusLost, ZeroMoney, ts(time.Hour))
		if err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if _, err := lost.Settle(WagerStatusWon, lost.PotentialPayout(), ts(2*time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("re-grading a settled wager: %v, want ErrIllegalTransition", err)
		}
		if _, err := lost.CashOut(mustMoney(t, "1.00"), ts(2*time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("cashing out a settled wager: %v, want ErrIllegalTransition", err)
		}
		if _, err := lost.Open(ts(2 * time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("reopening a settled wager: %v, want ErrIllegalTransition", err)
		}
		if _, err := lost.GradeLeg("leg-1", LegStatusWon, ts(2*time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("grading a leg of a settled wager: %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("an out-of-order update is refused", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		open, err := w.Open(ts(3 * time.Hour))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if _, err := open.Settle(WagerStatusLost, ZeroMoney, ts(time.Hour)); !errors.Is(err, ErrStaleUpdate) {
			t.Errorf("settling before the last update: %v, want ErrStaleUpdate", err)
		}
		if _, err := open.Settle(WagerStatusLost, ZeroMoney, time.Time{}); !errors.Is(err, ErrZeroTime) {
			t.Errorf("settling at the zero time: %v, want ErrZeroTime", err)
		}
	})

	t.Run("cashing out is not a grading", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		if _, err := w.Settle(WagerStatusCashedOut, mustMoney(t, "10.00"), ts(time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Errorf("Settle(cashed_out): %v, want ErrIllegalTransition", err)
		}
	})
}

func TestSettleChecksTheReturnedAmount(t *testing.T) {
	base := mustWager(t, straightParams(t))
	stake := base.Stake()
	payout := base.PotentialPayout()

	cases := []struct {
		name     string
		outcome  WagerStatus
		returned Money
		want     error
	}{
		{"a loser returns nothing", WagerStatusLost, ZeroMoney, nil},
		{"a loser cannot return money", WagerStatusLost, mustMoney(t, "0.01"), ErrReturnAmount},
		{"a push returns the stake", WagerStatusPush, stake, nil},
		{"a push cannot return more", WagerStatusPush, payout, ErrReturnAmount},
		{"a void returns the stake", WagerStatusVoid, stake, nil},
		{"a void cannot return less", WagerStatusVoid, mustMoney(t, "1.00"), ErrReturnAmount},
		{"a winner returns its payout", WagerStatusWon, payout, nil},
		{"a partially voided winner returns less", WagerStatusWon, mustMoney(t, "150.00"), nil},
		{"a winner cannot return below the stake", WagerStatusWon, mustMoney(t, "99.99"), ErrReturnAmount},
		{"a winner cannot return above the maximum", WagerStatusWon, mustMoney(t, "190.92"), ErrReturnAmount},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			settled, err := base.Settle(c.outcome, c.returned, ts(time.Hour))
			if c.want != nil {
				if !errors.Is(err, c.want) {
					t.Fatalf("Settle: %v, want %v", err, c.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("Settle: %v", err)
			}
			got, _ := settled.Returned()
			if got.Compare(c.returned) != 0 {
				t.Errorf("Returned() = %s, want %s", got, c.returned)
			}
			net, _ := settled.NetReturn()
			want, err := c.returned.Sub(stake)
			if err != nil {
				t.Fatalf("Sub: %v", err)
			}
			if net.Compare(want) != 0 {
				t.Errorf("NetReturn() = %s, want %s", net, want)
			}
		})
	}
}

func TestCashOut(t *testing.T) {
	w := mustWager(t, straightParams(t))

	t.Run("a price inside the range is accepted", func(t *testing.T) {
		out, err := w.CashOut(mustMoney(t, "120.00"), ts(time.Hour))
		if err != nil {
			t.Fatalf("CashOut: %v", err)
		}
		if out.Status() != WagerStatusCashedOut {
			t.Errorf("status = %v, want cashed_out", out.Status())
		}
		net, _ := out.NetReturn()
		if net.Compare(mustMoney(t, "20.00")) != 0 {
			t.Errorf("NetReturn() = %s, want 20.00", net)
		}
	})

	t.Run("a price below the stake is a real outcome", func(t *testing.T) {
		out, err := w.CashOut(mustMoney(t, "40.00"), ts(time.Hour))
		if err != nil {
			t.Fatalf("CashOut: %v", err)
		}
		net, _ := out.NetReturn()
		if net.Compare(mustMoney(t, "-60.00")) != 0 {
			t.Errorf("NetReturn() = %s, want -60.00", net)
		}
	})

	t.Run("zero and above-maximum are refused", func(t *testing.T) {
		if _, err := w.CashOut(ZeroMoney, ts(time.Hour)); !errors.Is(err, ErrReturnAmount) {
			t.Errorf("CashOut(0): %v, want ErrReturnAmount", err)
		}
		if _, err := w.CashOut(mustMoney(t, "190.92"), ts(time.Hour)); !errors.Is(err, ErrReturnAmount) {
			t.Errorf("CashOut above the maximum: %v, want ErrReturnAmount", err)
		}
	})
}

func TestGradeLeg(t *testing.T) {
	spread := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	total := mustLeg(t, totalSpec(t, "leg-2", "evt-2", "mkt-2"))
	w := mustWager(t, WagerParams{
		ID: "wgr-1", UserID: "usr-1", Kind: WagerKindParlay,
		Legs: []Leg{spread, total}, Stake: mustMoney(t, "50.00"),
		AcceptedDecimal: spread.QuotedDecimal() * total.QuotedDecimal(),
		Rounding:        RoundHalfAwayFromZero, PlacedAt: ts(0),
	})

	if w.AllLegsGraded() {
		t.Error("a fresh parlay reports all legs graded")
	}

	first, err := w.GradeLeg("leg-1", LegStatusWon, ts(3*time.Hour))
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	if first.AllLegsGraded() {
		t.Error("one graded leg of two reports all graded")
	}
	if w.Legs()[0].Status() != LegStatusPending {
		t.Error("GradeLeg mutated the receiver")
	}

	second, err := first.GradeLeg("leg-2", LegStatusVoid, ts(6*time.Hour))
	if err != nil {
		t.Fatalf("GradeLeg: %v", err)
	}
	if !second.AllLegsGraded() {
		t.Error("both legs graded but AllLegsGraded is false")
	}

	// The parlay rule: a void leg reprices the ticket as if it were never
	// added, so the combination collapses to the surviving leg's price.
	product := 1.0
	for _, leg := range second.Legs() {
		m, err := leg.GradedMultiplier()
		if err != nil {
			t.Fatalf("GradedMultiplier: %v", err)
		}
		product *= m
	}
	assertApprox(t, "graded combination", product, priceMinus110)

	if _, err := second.GradeLeg("leg-absent", LegStatusWon, ts(7*time.Hour)); !errors.Is(err, ErrMismatchedParent) {
		t.Errorf("grading a leg that is not on the ticket: %v, want ErrMismatchedParent", err)
	}
	if _, err := second.GradeLeg("leg-1", LegStatusLost, ts(7*time.Hour)); !errors.Is(err, ErrIllegalTransition) {
		t.Errorf("re-grading a graded leg: %v, want ErrIllegalTransition", err)
	}
}

// ---------------------------------------------------------------------------
// Round robins
// ---------------------------------------------------------------------------

func roundRobinParams(t *testing.T, n int, sizes []int) RoundRobinParams {
	t.Helper()
	return RoundRobinParams{
		ID:                  "rr-1",
		UserID:              "usr-1",
		Legs:                distinctLegs(t, n),
		Sizes:               sizes,
		StakePerCombination: mustMoney(t, "5.00"),
		PlacedAt:            ts(0),
	}
}

func TestRoundRobinExpansion(t *testing.T) {
	cases := []struct {
		name  string
		legs  int
		sizes []int
		want  int
	}{
		{"4 by 2s", 4, []int{2}, 6},
		{"4 by 3s", 4, []int{3}, 4},
		{"4 by 2s and 3s", 4, []int{2, 3}, 10},
		{"3 by 2s", 3, []int{2}, 3},
		{"8 by 4s", 8, []int{4}, 70},
		{"duplicate sizes collapse", 4, []int{2, 2, 2}, 6},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr, err := NewRoundRobin(roundRobinParams(t, c.legs, c.sizes))
			if err != nil {
				t.Fatalf("NewRoundRobin: %v", err)
			}

			// Cross-check the count against an independent binomial.
			independent := 0
			for _, k := range rr.Sizes() {
				independent += binomial(c.legs, k)
			}
			if independent != c.want {
				t.Fatalf("the test's own expectation is wrong: C sum = %d, want %d", independent, c.want)
			}
			if got := rr.CombinationCount(); got != c.want {
				t.Errorf("CombinationCount() = %d, want %d", got, c.want)
			}

			combos := rr.Combinations()
			if len(combos) != c.want {
				t.Fatalf("Combinations() returned %d, want %d", len(combos), c.want)
			}

			seen := make(map[string]struct{}, len(combos))
			for _, combo := range combos {
				if len(combo) < 2 {
					t.Fatalf("a combination has %d legs", len(combo))
				}
				key := ""
				ids := make([]string, 0, len(combo))
				for _, leg := range combo {
					ids = append(ids, string(leg.ID()))
				}
				if !slices.IsSorted(ids) {
					t.Errorf("combination %v is not in selection order", ids)
				}
				for _, id := range ids {
					key += id + "|"
				}
				if _, dup := seen[key]; dup {
					t.Errorf("combination %v was generated twice", ids)
				}
				seen[key] = struct{}{}
			}

			total, err := rr.TotalStake()
			if err != nil {
				t.Fatalf("TotalStake: %v", err)
			}
			want, err := rr.StakePerCombination().MulInt(int64(c.want))
			if err != nil {
				t.Fatalf("MulInt: %v", err)
			}
			if total.Compare(want) != 0 {
				t.Errorf("TotalStake() = %s, want %s", total, want)
			}

			// The expansion is deterministic.
			again := rr.Combinations()
			for i := range combos {
				for j := range combos[i] {
					if !combos[i][j].Equal(again[i][j]) {
						t.Fatalf("combination %d leg %d differs between calls", i, j)
					}
				}
			}
		})
	}
}

// TestRoundRobinTicketsAreOrdinaryWagers walks the relationship the charter
// asks to be explicit: a round robin expands into independent tickets, each
// naming it as parent.
func TestRoundRobinTicketsAreOrdinaryWagers(t *testing.T) {
	rr, err := NewRoundRobin(roundRobinParams(t, 4, []int{2}))
	if err != nil {
		t.Fatalf("NewRoundRobin: %v", err)
	}

	wagers := make([]Wager, 0, rr.CombinationCount())
	for i, combo := range rr.Combinations() {
		price := 1.0
		for _, leg := range combo {
			price *= leg.QuotedDecimal()
		}
		w, err := NewWager(WagerParams{
			ID:              WagerID("wgr-rr-" + strconv.Itoa(i)),
			UserID:          rr.UserID(),
			Kind:            WagerKindRoundRobin,
			Legs:            combo,
			Stake:           rr.StakePerCombination(),
			AcceptedDecimal: price,
			Rounding:        RoundHalfAwayFromZero,
			RoundRobinID:    rr.ID(),
			PlacedAt:        rr.PlacedAt(),
		})
		if err != nil {
			t.Fatalf("NewWager for combination %d: %v", i, err)
		}
		wagers = append(wagers, w)
	}

	if len(wagers) != 6 {
		t.Fatalf("expanded into %d tickets, want 6", len(wagers))
	}

	staked := ZeroMoney
	for _, w := range wagers {
		parent, ok := w.RoundRobinID()
		if !ok || parent != rr.ID() {
			t.Errorf("ticket %s names parent %s, %v", w.ID(), parent, ok)
		}
		next, err := staked.Add(w.Stake())
		if err != nil {
			t.Fatalf("Add: %v", err)
		}
		staked = next
	}
	total, err := rr.TotalStake()
	if err != nil {
		t.Fatalf("TotalStake: %v", err)
	}
	if staked.Compare(total) != 0 {
		t.Errorf("the tickets stake %s but the round robin says %s", staked, total)
	}

	// The tickets settle independently: one wins, one loses.
	won, err := wagers[0].Settle(WagerStatusWon, wagers[0].PotentialPayout(), ts(time.Hour))
	if err != nil {
		t.Fatalf("Settle: %v", err)
	}
	if wagers[1].Status() != WagerStatusPlaced {
		t.Error("settling one ticket changed another")
	}
	if won.Status() != WagerStatusWon {
		t.Errorf("status = %v", won.Status())
	}
}

func TestNewRoundRobinRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, p *RoundRobinParams)
		want   error
	}{
		{"empty id", func(_ *testing.T, p *RoundRobinParams) { p.ID = "" }, ErrEmptyID},
		{"empty user id", func(_ *testing.T, p *RoundRobinParams) { p.UserID = "" }, ErrEmptyID},
		{"one selection", func(t *testing.T, p *RoundRobinParams) {
			p.Legs = distinctLegs(t, 1)
			p.Sizes = []int{2}
		}, ErrLegCount},
		{"past the selection cap", func(t *testing.T, p *RoundRobinParams) {
			p.Legs = distinctLegs(t, MaxRoundRobinLegs+1)
		}, ErrLegCount},
		{"no sizes", func(_ *testing.T, p *RoundRobinParams) { p.Sizes = nil }, ErrCombinationSize},
		{"a size of one", func(_ *testing.T, p *RoundRobinParams) { p.Sizes = []int{1} }, ErrCombinationSize},
		{"a size past the selection count", func(_ *testing.T, p *RoundRobinParams) {
			p.Sizes = []int{9}
		}, ErrCombinationSize},
		{"a zero stake", func(_ *testing.T, p *RoundRobinParams) {
			p.StakePerCombination = ZeroMoney
		}, ErrStakeNotPositive},
		{"a zero placement time", func(_ *testing.T, p *RoundRobinParams) {
			p.PlacedAt = time.Time{}
		}, ErrZeroTime},
		{"a repeated selection", func(t *testing.T, p *RoundRobinParams) {
			legs := distinctLegs(t, 3)
			p.Legs = []Leg{legs[0], legs[1], legs[0]}
		}, ErrDuplicateSelection},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := roundRobinParams(t, 4, []int{2})
			c.mutate(t, &p)
			_, err := NewRoundRobin(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewRoundRobin: %v, want %v", err, c.want)
			}
		})
	}

	t.Run("the zero RoundRobin is detectable", func(t *testing.T) {
		if !(RoundRobin{}).IsZero() {
			t.Error("the zero RoundRobin does not report IsZero")
		}
		rr, err := NewRoundRobin(roundRobinParams(t, 4, []int{2}))
		if err != nil {
			t.Fatalf("NewRoundRobin: %v", err)
		}
		if rr.IsZero() {
			t.Error("a constructed RoundRobin reports IsZero")
		}
		if len(rr.Legs()) != 4 {
			t.Errorf("Legs() returned %d", len(rr.Legs()))
		}
		if s := rr.String(); s == "roundrobin(<zero>)" {
			t.Errorf("String() = %q on a constructed value", s)
		}
	})
}

// TestCombinationIndexes checks the expansion primitive directly: the right
// count, strictly increasing indices, and lexicographic order.
func TestCombinationIndexes(t *testing.T) {
	for n := 1; n <= 8; n++ {
		for k := 0; k <= n+1; k++ {
			got := combinationIndexes(n, k)
			want := 0
			if k >= 1 && k <= n {
				want = binomial(n, k)
			}
			if len(got) != want {
				t.Fatalf("combinationIndexes(%d, %d) returned %d sets, want %d", n, k, len(got), want)
			}
			for i, idx := range got {
				if len(idx) != k {
					t.Fatalf("set %d has %d indices, want %d", i, len(idx), k)
				}
				for j := 1; j < len(idx); j++ {
					if idx[j] <= idx[j-1] {
						t.Fatalf("set %v is not strictly increasing", idx)
					}
				}
				if idx[len(idx)-1] >= n {
					t.Fatalf("set %v indexes past %d", idx, n)
				}
				if i > 0 && slices.Compare(got[i-1], idx) >= 0 {
					t.Fatalf("set %v does not follow %v lexicographically", idx, got[i-1])
				}
			}
		}
	}
}

// TestWagerIdentifiersAndAccessors covers the identifier constructors and the
// accessors the other tests reach for only indirectly.
func TestWagerIdentifiersAndAccessors(t *testing.T) {
	user, err := NewUserID("usr-1")
	if err != nil {
		t.Fatalf("NewUserID: %v", err)
	}
	wager, err := NewWagerID("wgr-1")
	if err != nil {
		t.Fatalf("NewWagerID: %v", err)
	}
	rr, err := NewRoundRobinID("rr-1")
	if err != nil {
		t.Fatalf("NewRoundRobinID: %v", err)
	}
	if user.String() != "usr-1" || wager.String() != "wgr-1" || rr.String() != "rr-1" {
		t.Errorf("identifiers round trip as %q %q %q", user, wager, rr)
	}
	if user.IsZero() || wager.IsZero() || rr.IsZero() {
		t.Error("a constructed identifier reports IsZero")
	}
	if !UserID("").IsZero() || !WagerID("").IsZero() || !RoundRobinID("").IsZero() {
		t.Error("the empty identifier does not report IsZero")
	}
	for _, err := range []error{
		errOf(NewUserID("usr:1")),
		errOf(NewWagerID("")),
		errOf(NewRoundRobinID("rr 1")),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%v does not reach ErrInvalid", err)
		}
	}

	w := mustWager(t, straightParams(t))
	if w.Kind() != WagerKindStraight {
		t.Errorf("Kind() = %v", w.Kind())
	}
	if w.IsTerminal() {
		t.Error("a placed wager reports terminal")
	}
	if s := w.String(); s == "wager(<zero>)" {
		t.Errorf("String() = %q on a constructed wager", s)
	}
	if s := (Wager{}).String(); s != "wager(<zero>)" {
		t.Errorf("zero Wager String() = %q", s)
	}
	if got := WagerKindUnknown.String(); got != "unknown" {
		t.Errorf("WagerKindUnknown.String() = %q", got)
	}
	if got := WagerStatusUnknown.String(); got != "unknown" {
		t.Errorf("WagerStatusUnknown.String() = %q", got)
	}
	if WagerKindUnknown.Valid() || WagerStatusUnknown.Valid() {
		t.Error("a zero enum value reports as valid")
	}
	if WagerStatusUnknown.IsTerminal() || WagerStatusUnknown.IsGraded() || WagerStatusUnknown.HoldsEscrow() {
		t.Error("the zero WagerStatus satisfies a predicate")
	}
}

// errOf discards the value half of a (T, error) pair so a table can hold the
// errors from constructors with different value types.
func errOf[T any](_ T, err error) error { return err }

// TestWagerDefensivePaths reaches the branches the happy paths cannot.
func TestWagerDefensivePaths(t *testing.T) {
	t.Run("a potential payout that overflows is refused", func(t *testing.T) {
		stake, err := FromMinorUnits(int64(MaxSafeMoney))
		if err != nil {
			t.Fatalf("FromMinorUnits: %v", err)
		}
		_, err = NewWager(WagerParams{
			ID: "wgr-big", UserID: "usr-1", Kind: WagerKindParlay,
			Legs: distinctLegs(t, 2), Stake: stake, AcceptedDecimal: 1e6,
			Rounding: RoundHalfAwayFromZero, PlacedAt: ts(0),
		})
		if !errors.Is(err, ErrMoneyOverflow) {
			t.Fatalf("NewWager: %v, want ErrMoneyOverflow", err)
		}
	})

	t.Run("an unconstructed leg is refused", func(t *testing.T) {
		p := straightParams(t)
		p.Legs = []Leg{{}}
		if _, err := NewWager(p); !errors.Is(err, ErrLegPriceRequired) {
			t.Fatalf("NewWager: %v, want ErrLegPriceRequired", err)
		}
	})

	t.Run("the return check refuses a non-graded outcome", func(t *testing.T) {
		w := mustWager(t, straightParams(t))
		if err := w.checkReturn(WagerStatusOpen, ZeroMoney); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("checkReturn(open): %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("a round robin whose total stake overflows says so", func(t *testing.T) {
		stake, err := FromMinorUnits(int64(MaxSafeMoney))
		if err != nil {
			t.Fatalf("FromMinorUnits: %v", err)
		}
		rr, err := NewRoundRobin(RoundRobinParams{
			ID: "rr-big", UserID: "usr-1", Legs: distinctLegs(t, MaxRoundRobinLegs),
			Sizes: []int{5}, StakePerCombination: stake, PlacedAt: ts(0),
		})
		if err != nil {
			t.Fatalf("NewRoundRobin: %v", err)
		}
		if got := rr.CombinationCount(); got != binomial(MaxRoundRobinLegs, 5) {
			t.Fatalf("CombinationCount() = %d", got)
		}
		if _, err := rr.TotalStake(); !errors.Is(err, ErrMoneyOverflow) {
			t.Fatalf("TotalStake: %v, want ErrMoneyOverflow", err)
		}
	})

	t.Run("the zero RoundRobin renders", func(t *testing.T) {
		if s := (RoundRobin{}).String(); s != "roundrobin(<zero>)" {
			t.Errorf("String() = %q", s)
		}
	})

	t.Run("UnmarshalText rejects undefined text", func(t *testing.T) {
		var kind WagerKind
		if err := kind.UnmarshalText([]byte("accumulator")); !errors.Is(err, ErrUnknownWagerKind) {
			t.Errorf("WagerKind.UnmarshalText: %v", err)
		}
		var status WagerStatus
		if err := status.UnmarshalText([]byte("graded")); !errors.Is(err, ErrUnknownWagerStatus) {
			t.Errorf("WagerStatus.UnmarshalText: %v", err)
		}
	})
}
