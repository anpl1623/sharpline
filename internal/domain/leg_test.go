package domain

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Shared betting fixtures
// ---------------------------------------------------------------------------

// Decimal forms of the American prices these tests are written against.
//
// The values are real market conventions, not invented numbers: -110 is the
// standard US juice on a two-sided market, -105 is a reduced-juice book's
// version of it, and +150 / -200 are an ordinary moneyline pair. They are
// written as the exact arithmetic that defines them rather than as decimal
// literals, so no rounding is baked into the fixture.
//
// As in doc_test.go, no assertion here claims to reproduce a specific
// historical quote from a specific book on a specific date. That claim needs a
// recorded provider payload, which is what the ingest phase's golden files are
// for.
const (
	// priceMinus110 is American -110: risk 110 to win 100, so the total return
	// per unit staked is 1 + 100/110.
	priceMinus110 = 1 + 100.0/110.0

	// priceMinus105 is American -105.
	priceMinus105 = 1 + 100.0/105.0

	// pricePlus150 is American +150: risk 100 to win 150.
	pricePlus150 = 2.5

	// priceMinus200 is American -200: risk 200 to win 100.
	priceMinus200 = 1.5
)

// betLegSpec describes one leg for a test to build, together with the market
// and selection it hangs off. Everything downstream of the leg — wagers, round
// robins, ledger movements — is built from these, so a change to the domain's
// construction rules surfaces in one place.
type betLegSpec struct {
	legID    string
	eventID  string
	marketID string
	typ      MarketType
	line     Line
	role     SelectionRole
	decimal  float64
}

// build returns the market, selection, price, and leg the spec describes,
// wired together through the real constructors and cross-validated by
// NewLegFrom.
func (s betLegSpec) build(t *testing.T) (Market, Selection, Price, Leg) {
	t.Helper()

	market, err := NewMarket(MarketParams{
		ID:        MarketID(s.marketID),
		EventID:   EventID(s.eventID),
		Type:      s.typ,
		Line:      s.line,
		Status:    MarketStatusOpen,
		UpdatedAt: ts(0),
	})
	if err != nil {
		t.Fatalf("NewMarket(%s): %v", s.marketID, err)
	}
	selection, err := NewSelection(SelectionParams{
		ID:       SelectionID(s.marketID + "-" + s.role.String()),
		MarketID: market.ID(),
		Role:     s.role,
		Name:     s.marketID + " " + s.role.String(),
	})
	if err != nil {
		t.Fatalf("NewSelection(%s): %v", s.marketID, err)
	}
	effective, err := EffectiveLine(market, selection)
	if err != nil {
		t.Fatalf("EffectiveLine(%s): %v", s.marketID, err)
	}
	price, err := NewPrice(PriceParams{
		SelectionID: selection.ID(),
		BookID:      "book-sharpline",
		Decimal:     s.decimal,
		Line:        effective,
		ObservedAt:  ts(time.Minute),
	})
	if err != nil {
		t.Fatalf("NewPrice(%s): %v", s.marketID, err)
	}
	leg, err := NewLegFrom(LegID(s.legID), market, selection, price)
	if err != nil {
		t.Fatalf("NewLegFrom(%s): %v", s.legID, err)
	}
	return market, selection, price, leg
}

// mustLeg returns just the leg the spec describes.
func mustLeg(t *testing.T, s betLegSpec) Leg {
	t.Helper()
	_, _, _, leg := s.build(t)
	return leg
}

// spreadSpec is the canonical two-sided spread leg: a home side laying 3.5 at
// standard -110 juice.
func spreadSpec(t *testing.T, legID, eventID, marketID string) betLegSpec {
	t.Helper()
	return betLegSpec{
		legID:    legID,
		eventID:  eventID,
		marketID: marketID,
		typ:      MarketTypeSpread,
		line:     mustLine(t, -3.5),
		role:     SelectionRoleHome,
		decimal:  priceMinus110,
	}
}

// totalSpec is the canonical total leg: over 47.5 at -110.
func totalSpec(t *testing.T, legID, eventID, marketID string) betLegSpec {
	t.Helper()
	return betLegSpec{
		legID:    legID,
		eventID:  eventID,
		marketID: marketID,
		typ:      MarketTypeTotal,
		line:     mustLine(t, 47.5),
		role:     SelectionRoleOver,
		decimal:  priceMinus110,
	}
}

// moneylineSpec is a moneyline leg, which carries no line at all.
func moneylineSpec(legID, eventID, marketID string, decimal float64) betLegSpec {
	return betLegSpec{
		legID:    legID,
		eventID:  eventID,
		marketID: marketID,
		typ:      MarketTypeMoneyline,
		line:     NoLine(),
		role:     SelectionRoleHome,
		decimal:  decimal,
	}
}

// ---------------------------------------------------------------------------
// LegStatus
// ---------------------------------------------------------------------------

func TestLegStatusTextRoundTrip(t *testing.T) {
	cases := []struct {
		status LegStatus
		text   string
	}{
		{LegStatusPending, "pending"},
		{LegStatusWon, "won"},
		{LegStatusLost, "lost"},
		{LegStatusVoid, "void"},
		{LegStatusPush, "push"},
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			if got := c.status.String(); got != c.text {
				t.Errorf("String() = %q, want %q", got, c.text)
			}
			parsed, err := ParseLegStatus(c.text)
			if err != nil {
				t.Fatalf("ParseLegStatus(%q): %v", c.text, err)
			}
			if parsed != c.status {
				t.Errorf("ParseLegStatus(%q) = %v, want %v", c.text, parsed, c.status)
			}
			b, err := c.status.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var back LegStatus
			if err := back.UnmarshalText(b); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", b, err)
			}
			if back != c.status {
				t.Errorf("round trip = %v, want %v", back, c.status)
			}
		})
	}

	if _, err := ParseLegStatus("cashed_out"); !errors.Is(err, ErrUnknownLegStatus) {
		t.Errorf("ParseLegStatus of a wager-only status: %v, want ErrUnknownLegStatus", err)
	}
	if _, err := LegStatusUnknown.MarshalText(); !errors.Is(err, ErrUnknownLegStatus) {
		t.Errorf("MarshalText of the zero value: %v, want ErrUnknownLegStatus", err)
	}
}

// TestLegStatusTransitionMatrix asserts every ordered pair against an explicit
// table of legal edges rather than against a re-derivation of the rule, so the
// test would still fail if the implementation and the rule changed together.
func TestLegStatusTransitionMatrix(t *testing.T) {
	all := []LegStatus{
		LegStatusUnknown, LegStatusPending, LegStatusWon,
		LegStatusLost, LegStatusVoid, LegStatusPush,
	}
	allowed := map[LegStatus][]LegStatus{
		LegStatusPending: {LegStatusPending, LegStatusWon, LegStatusLost, LegStatusVoid, LegStatusPush},
		LegStatusWon:     {LegStatusWon},
		LegStatusLost:    {LegStatusLost},
		LegStatusVoid:    {LegStatusVoid},
		LegStatusPush:    {LegStatusPush},
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

func TestNewLegRejectsBadInput(t *testing.T) {
	_, _, price, good := spreadSpec(t, "leg-1", "evt-1", "mkt-1").build(t)

	base := LegParams{
		ID:          "leg-1",
		EventID:     "evt-1",
		MarketID:    "mkt-1",
		MarketType:  MarketTypeSpread,
		Role:        SelectionRoleHome,
		SelectionID: good.SelectionID(),
		Price:       price,
	}

	cases := []struct {
		name   string
		mutate func(p *LegParams)
		want   error
	}{
		{"empty id", func(p *LegParams) { p.ID = "" }, ErrEmptyID},
		{"id with a colon", func(p *LegParams) { p.ID = "leg:1" }, ErrIDCharset},
		{"empty event id", func(p *LegParams) { p.EventID = "" }, ErrEmptyID},
		{"empty market id", func(p *LegParams) { p.MarketID = "" }, ErrEmptyID},
		{"empty selection id", func(p *LegParams) { p.SelectionID = "" }, ErrEmptyID},
		{"unset market type", func(p *LegParams) { p.MarketType = MarketTypeUnknown }, ErrUnknownMarketType},
		{"unset role", func(p *LegParams) { p.Role = SelectionRoleUnknown }, ErrUnknownSelectionRole},
		{"role the market type forbids", func(p *LegParams) { p.Role = SelectionRoleOver }, ErrRoleNotApplicable},
		{"no price", func(p *LegParams) { p.Price = Price{} }, ErrLegPriceRequired},
		{"price quoting another selection", func(p *LegParams) { p.SelectionID = "sel-other" }, ErrLegPriceMismatch},
		{"teasing a leg whose price has no line", func(p *LegParams) {
			ml := mustLeg(t, moneylineSpec("leg-ml", "evt-1", "mkt-ml", pricePlus150))
			p.MarketType = MarketTypeMoneyline
			p.SelectionID = ml.SelectionID()
			p.Price = ml.Price()
			p.TeasedLine = mustLine(t, 2.5)
		}, ErrLegNotTeasable},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mutate(&p)
			_, err := NewLeg(p)
			if !errors.Is(err, c.want) {
				t.Fatalf("NewLeg: %v, want %v", err, c.want)
			}
			if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrConflict) {
				t.Errorf("error %v reaches neither taxonomy root", err)
			}
		})
	}

	t.Run("a valid leg is born pending and ungraded", func(t *testing.T) {
		leg, err := NewLeg(base)
		if err != nil {
			t.Fatalf("NewLeg: %v", err)
		}
		if leg.Status() != LegStatusPending {
			t.Errorf("status = %v, want pending", leg.Status())
		}
		if _, graded := leg.GradedAt(); graded {
			t.Error("a freshly constructed leg reports a grading time")
		}
		if leg.IsZero() {
			t.Error("a constructed leg reports IsZero")
		}
		if !(Leg{}).IsZero() {
			t.Error("the zero Leg does not report IsZero")
		}
	})
}

// TestNewLegFromRejectsAStaleLine is the cross-validation NewLegFrom exists
// for: a price taken at a line the selection no longer trades at would grade
// the customer at a handicap they never took.
func TestNewLegFromRejectsAStaleLine(t *testing.T) {
	market, selection, price, _ := spreadSpec(t, "leg-1", "evt-1", "mkt-1").build(t)

	moved, err := market.WithLine(mustLine(t, -4.5), ts(2*time.Minute))
	if err != nil {
		t.Fatalf("Market.WithLine: %v", err)
	}
	if _, err := NewLegFrom("leg-2", moved, selection, price); !errors.Is(err, ErrLineMismatch) {
		t.Fatalf("NewLegFrom against a moved line: %v, want ErrLineMismatch", err)
	}

	// The same price against the market it was actually taken on is fine.
	if _, err := NewLegFrom("leg-3", market, selection, price); err != nil {
		t.Fatalf("NewLegFrom on a consistent triple: %v", err)
	}
}

// TestNewLegFromAppliesTheHomePerspectiveConvention checks that an away spread
// leg is booked at the inverted line, which is EffectiveLine's whole purpose.
func TestNewLegFromAppliesTheHomePerspectiveConvention(t *testing.T) {
	spec := spreadSpec(t, "leg-away", "evt-1", "mkt-1")
	spec.role = SelectionRoleAway
	_, _, _, leg := spec.build(t)

	got, ok := leg.GradingLine().Value()
	if !ok {
		t.Fatal("an away spread leg has no grading line")
	}
	assertApprox(t, "away leg grading line", got, 3.5)
}

// ---------------------------------------------------------------------------
// The copied price
// ---------------------------------------------------------------------------

// TestLegHoldsThePriceAtPlacement is the test the whole type exists for
// (CLAUDE.md §4: "Legs hold the price *at placement time*, never a live
// reference"). It moves the market underneath a booked leg and asserts nothing
// the leg reports changes.
func TestLegHoldsThePriceAtPlacement(t *testing.T) {
	market, selection, price, leg := spreadSpec(t, "leg-1", "evt-1", "mkt-1").build(t)

	bookedDecimal := leg.QuotedDecimal()
	bookedLine, _ := leg.GradingLine().Value()

	// The line moves and the price with it — a steam move on the home side.
	moved, err := market.WithLine(mustLine(t, -6.5), ts(5*time.Minute))
	if err != nil {
		t.Fatalf("Market.WithLine: %v", err)
	}
	effective, err := EffectiveLine(moved, selection)
	if err != nil {
		t.Fatalf("EffectiveLine: %v", err)
	}
	newer, err := NewPrice(PriceParams{
		SelectionID: selection.ID(),
		BookID:      price.BookID(),
		Decimal:     priceMinus105,
		Line:        effective,
		ObservedAt:  ts(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("NewPrice: %v", err)
	}
	if !newer.IsNewerThan(price) {
		t.Fatal("the fixture did not actually produce a later price")
	}

	// Nothing about the booked leg moved.
	assertApprox(t, "booked decimal after the market moved", leg.QuotedDecimal(), bookedDecimal)
	gotLine, ok := leg.GradingLine().Value()
	if !ok {
		t.Fatal("the booked leg lost its line")
	}
	assertApprox(t, "booked line after the market moved", gotLine, bookedLine)
	if !leg.Price().Equal(price) {
		t.Errorf("leg price = %v, want the price it was booked at %v", leg.Price(), price)
	}

	// And a state change returns a new value rather than editing the old one.
	graded, err := leg.WithStatus(LegStatusWon, ts(3*time.Hour))
	if err != nil {
		t.Fatalf("WithStatus: %v", err)
	}
	if leg.Status() != LegStatusPending {
		t.Errorf("the receiver was mutated: status = %v", leg.Status())
	}
	if graded.Status() != LegStatusWon {
		t.Errorf("the copy did not change: status = %v", graded.Status())
	}
	if !graded.Price().Equal(leg.Price()) {
		t.Error("grading altered the booked price")
	}
}

// ---------------------------------------------------------------------------
// Grading
// ---------------------------------------------------------------------------

func TestLegWithStatus(t *testing.T) {
	leg := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))

	t.Run("an illegal edge is refused", func(t *testing.T) {
		won, err := leg.WithStatus(LegStatusWon, ts(time.Hour))
		if err != nil {
			t.Fatalf("WithStatus(won): %v", err)
		}
		if _, err := won.WithStatus(LegStatusLost, ts(2*time.Hour)); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("won → lost: %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("a zero grading time is refused", func(t *testing.T) {
		if _, err := leg.WithStatus(LegStatusWon, time.Time{}); !errors.Is(err, ErrZeroTime) {
			t.Fatalf("WithStatus with the zero time: %v, want ErrZeroTime", err)
		}
	})

	t.Run("redelivery keeps the original grading time", func(t *testing.T) {
		first, err := leg.WithStatus(LegStatusVoid, ts(time.Hour))
		if err != nil {
			t.Fatalf("WithStatus: %v", err)
		}
		again, err := first.WithStatus(LegStatusVoid, ts(9*time.Hour))
		if err != nil {
			t.Fatalf("redelivered WithStatus: %v", err)
		}
		got, ok := again.GradedAt()
		if !ok {
			t.Fatal("a graded leg reports no grading time")
		}
		if !got.Equal(ts(time.Hour)) {
			t.Errorf("grading time = %s, want the original %s", got, ts(time.Hour))
		}
	})

	t.Run("an undefined status is refused", func(t *testing.T) {
		if _, err := leg.WithStatus(LegStatusUnknown, ts(time.Hour)); !errors.Is(err, ErrUnknownLegStatus) {
			t.Fatalf("WithStatus(unknown): %v", err)
		}
	})
}

func TestLegGradedMultiplier(t *testing.T) {
	leg := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))

	cases := []struct {
		status LegStatus
		want   float64
	}{
		{LegStatusWon, priceMinus110},
		{LegStatusLost, 0},
		{LegStatusVoid, 1},
		{LegStatusPush, 1},
	}
	for _, c := range cases {
		t.Run(c.status.String(), func(t *testing.T) {
			graded, err := leg.WithStatus(c.status, ts(time.Hour))
			if err != nil {
				t.Fatalf("WithStatus(%v): %v", c.status, err)
			}
			got, err := graded.GradedMultiplier()
			if err != nil {
				t.Fatalf("GradedMultiplier: %v", err)
			}
			assertApprox(t, "graded multiplier", got, c.want)
		})
	}

	t.Run("a pending leg refuses to guess", func(t *testing.T) {
		_, err := leg.GradedMultiplier()
		if !errors.Is(err, ErrLegNotGraded) {
			t.Fatalf("GradedMultiplier on a pending leg: %v, want ErrLegNotGraded", err)
		}
		if !errors.Is(err, ErrConflict) {
			t.Errorf("%v does not reach ErrConflict", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Teasing
// ---------------------------------------------------------------------------

func TestLegWithTeasedLine(t *testing.T) {
	spread := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	total := mustLeg(t, totalSpec(t, "leg-2", "evt-2", "mkt-2"))
	moneyline := mustLeg(t, moneylineSpec("leg-3", "evt-3", "mkt-3", pricePlus150))

	t.Run("a spread teases and keeps its booked price", func(t *testing.T) {
		// A 6-point teaser moves -3.5 to +2.5.
		teased, err := spread.WithTeasedLine(mustLine(t, 2.5))
		if err != nil {
			t.Fatalf("WithTeasedLine: %v", err)
		}
		got, ok := teased.GradingLine().Value()
		if !ok {
			t.Fatal("the teased leg has no grading line")
		}
		assertApprox(t, "teased grading line", got, 2.5)

		booked, ok := teased.Price().Line().Value()
		if !ok {
			t.Fatal("teasing destroyed the booked line")
		}
		assertApprox(t, "booked line survives the tease", booked, -3.5)
		assertApprox(t, "booked price survives the tease", teased.QuotedDecimal(), priceMinus110)
	})

	t.Run("a total teases", func(t *testing.T) {
		teased, err := total.WithTeasedLine(mustLine(t, 41.5))
		if err != nil {
			t.Fatalf("WithTeasedLine: %v", err)
		}
		got, _ := teased.GradingLine().Value()
		assertApprox(t, "teased total", got, 41.5)
	})

	t.Run("a moneyline cannot be teased", func(t *testing.T) {
		if _, err := moneyline.WithTeasedLine(mustLine(t, 2.5)); !errors.Is(err, ErrLegNotTeasable) {
			t.Fatalf("teasing a moneyline: %v, want ErrLegNotTeasable", err)
		}
	})

	t.Run("an absent teased line is refused", func(t *testing.T) {
		if _, err := spread.WithTeasedLine(NoLine()); !errors.Is(err, ErrLineRequired) {
			t.Fatalf("teasing to no line: %v, want ErrLineRequired", err)
		}
	})

	t.Run("a graded leg cannot be teased", func(t *testing.T) {
		graded, err := spread.WithStatus(LegStatusWon, ts(time.Hour))
		if err != nil {
			t.Fatalf("WithStatus: %v", err)
		}
		if _, err := graded.WithTeasedLine(mustLine(t, 2.5)); !errors.Is(err, ErrIllegalTransition) {
			t.Fatalf("teasing a graded leg: %v, want ErrIllegalTransition", err)
		}
	})

	t.Run("NewLeg applies a teased line through the same path", func(t *testing.T) {
		leg, err := NewLeg(LegParams{
			ID:          "leg-teased",
			EventID:     "evt-1",
			MarketID:    "mkt-1",
			MarketType:  MarketTypeSpread,
			Role:        SelectionRoleHome,
			SelectionID: spread.SelectionID(),
			Price:       spread.Price(),
			TeasedLine:  mustLine(t, 2.5),
		})
		if err != nil {
			t.Fatalf("NewLeg with a teased line: %v", err)
		}
		got, _ := leg.TeasedLine().Value()
		assertApprox(t, "teased line", got, 2.5)
	})
}

// ---------------------------------------------------------------------------
// Value semantics
// ---------------------------------------------------------------------------

func TestLegEqual(t *testing.T) {
	a := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	b := mustLeg(t, spreadSpec(t, "leg-1", "evt-1", "mkt-1"))
	if !a.Equal(b) {
		t.Error("two legs built from the same spec are not equal")
	}

	other := mustLeg(t, totalSpec(t, "leg-1", "evt-1", "mkt-2"))
	if a.Equal(other) {
		t.Error("legs on different markets compare equal")
	}

	gradedEarly, err := a.WithStatus(LegStatusWon, ts(time.Hour))
	if err != nil {
		t.Fatalf("WithStatus: %v", err)
	}
	gradedLate, err := b.WithStatus(LegStatusWon, ts(2*time.Hour))
	if err != nil {
		t.Fatalf("WithStatus: %v", err)
	}
	if gradedEarly.Equal(gradedLate) {
		t.Error("legs graded at different instants compare equal")
	}
	if a.Equal(gradedEarly) {
		t.Error("a pending leg equals its graded copy")
	}
	if s := a.String(); s == "" || s == "leg(<zero>)" {
		t.Errorf("String() = %q on a constructed leg", s)
	}
	if s := (Leg{}).String(); s != "leg(<zero>)" {
		t.Errorf("zero Leg String() = %q", s)
	}
}

// TestLegIdentifierAndCopiedFields covers the identifier constructor and the
// two fields copied from the market and selection purely so the leg can be
// graded without re-reading either.
func TestLegIdentifierAndCopiedFields(t *testing.T) {
	id, err := NewLegID("leg-1")
	if err != nil {
		t.Fatalf("NewLegID: %v", err)
	}
	if id.String() != "leg-1" || id.IsZero() {
		t.Errorf("NewLegID gave %q", id)
	}
	if _, err := NewLegID("leg:1"); !errors.Is(err, ErrIDCharset) {
		t.Errorf("NewLegID with a colon: %v, want ErrIDCharset", err)
	}
	if _, err := NewLegID(""); !errors.Is(err, ErrEmptyID) {
		t.Errorf("NewLegID of the empty string: %v", err)
	}

	leg := mustLeg(t, totalSpec(t, "leg-1", "evt-1", "mkt-1"))
	if leg.MarketType() != MarketTypeTotal {
		t.Errorf("MarketType() = %v, want total", leg.MarketType())
	}
	if leg.Role() != SelectionRoleOver {
		t.Errorf("Role() = %v, want over", leg.Role())
	}
	if got := LegStatusUnknown.String(); got != "unknown" {
		t.Errorf("LegStatusUnknown.String() = %q", got)
	}
	if LegStatusUnknown.Valid() || LegStatusUnknown.IsTerminal() {
		t.Error("the zero LegStatus reports as valid or terminal")
	}
}

// TestLegDefensivePaths reaches the branches the happy paths cannot.
func TestLegDefensivePaths(t *testing.T) {
	t.Run("teasing a leg whose booked price carries no line", func(t *testing.T) {
		// NewLeg is also the rehydration path, so it accepts a spread leg whose
		// stored price lost its line. Teasing one is refused, because the tease
		// would have nothing to be measured against.
		ml := mustLeg(t, moneylineSpec("leg-ml", "evt-1", "mkt-ml", pricePlus150))
		leg, err := NewLeg(LegParams{
			ID: "leg-lineless", EventID: "evt-1", MarketID: "mkt-ml",
			MarketType: MarketTypeSpread, Role: SelectionRoleHome,
			SelectionID: ml.SelectionID(), Price: ml.Price(),
		})
		if err != nil {
			t.Fatalf("NewLeg: %v", err)
		}
		if _, err := leg.WithTeasedLine(mustLine(t, 2.5)); !errors.Is(err, ErrLegLineRequired) {
			t.Fatalf("WithTeasedLine: %v, want ErrLegLineRequired", err)
		}
	})

	t.Run("UnmarshalText rejects undefined text", func(t *testing.T) {
		var s LegStatus
		if err := s.UnmarshalText([]byte("settled")); !errors.Is(err, ErrUnknownLegStatus) {
			t.Fatalf("UnmarshalText: %v, want ErrUnknownLegStatus", err)
		}
	})
}
