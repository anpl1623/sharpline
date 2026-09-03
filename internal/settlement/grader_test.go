// Tests for the pure grading function.
//
// They are exhaustive rather than representative, and deliberately so: this is
// the function that decides who gets paid, it has no dependencies and no state,
// and there is therefore no excuse for testing it by sampling. Every market
// type, every role that type admits, and every side of every boundary — won,
// lost, and landing exactly on the number — is enumerated below.
//
// The spread table is the one worth reading closely. It asserts the away side's
// sign convention explicitly and at the same numbers as the home side, because
// grading the away leg against the home leg's handicap is the single most
// plausible bug in this file and it produces a wrong answer on every game that
// is not a pick'em rather than an error anybody would see.
package settlement

import (
	"errors"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// -----------------------------------------------------------------------------
// Fixtures
// -----------------------------------------------------------------------------

// finalisedAt is the instant every fixture result is stamped with. A fixed
// instant rather than time.Now, so a failure is reproducible and so nothing in
// these tests can accidentally depend on the wall clock.
var finalisedAt = time.Date(2026, 8, 20, 22, 15, 0, 0, time.UTC)

func mustLine(t *testing.T, v float64) domain.Line {
	t.Helper()
	l, err := domain.NewLine(v)
	if err != nil {
		t.Fatalf("domain.NewLine(%v): %v", v, err)
	}
	return l
}

func mustScore(t *testing.T, home, away int) domain.Score {
	t.Helper()
	s, err := domain.NewScore(home, away)
	if err != nil {
		t.Fatalf("domain.NewScore(%d, %d): %v", home, away, err)
	}
	return s
}

// finalResult is a played-to-conclusion event.
func finalResult(t *testing.T, event string, home, away int) Result {
	t.Helper()
	return Result{
		EventID:     domain.EventID(event),
		Status:      domain.EventStatusEnded,
		Score:       mustScore(t, home, away),
		HasScore:    true,
		FinalisedAt: finalisedAt,
	}
}

// cancelledResult is an event that will never produce one.
func cancelledResult(event string) Result {
	return Result{
		EventID:     domain.EventID(event),
		Status:      domain.EventStatusCancelled,
		FinalisedAt: finalisedAt,
	}
}

// legRef builds a valid grading input for one leg.
func legRef(event string, typ domain.MarketType, role domain.SelectionRole, line domain.Line) LegRef {
	return LegRef{
		LegID:       "leg-1",
		WagerID:     "wager-1",
		EventID:     domain.EventID(event),
		MarketType:  typ,
		Role:        role,
		GradingLine: line,
	}
}

// -----------------------------------------------------------------------------
// Moneyline
// -----------------------------------------------------------------------------

// TestGradeMarketMoneylineTwoWay covers the book that quotes no draw: a tie is a
// PUSH for both sides and the stake comes back, because there was never a third
// price to take.
func TestGradeMarketMoneylineTwoWay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		role       domain.SelectionRole
		home, away int
		want       domain.LegStatus
	}{
		{"home backed, home wins", domain.SelectionRoleHome, 24, 20, domain.LegStatusWon},
		{"home backed, away wins", domain.SelectionRoleHome, 20, 24, domain.LegStatusLost},
		{"home backed, tied", domain.SelectionRoleHome, 21, 21, domain.LegStatusPush},
		{"away backed, away wins", domain.SelectionRoleAway, 20, 24, domain.LegStatusWon},
		{"away backed, home wins", domain.SelectionRoleAway, 24, 20, domain.LegStatusLost},
		{"away backed, tied", domain.SelectionRoleAway, 21, 21, domain.LegStatusPush},
		{"goalless tie is still a tie", domain.SelectionRoleHome, 0, 0, domain.LegStatusPush},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := GradeMarket(domain.MarketTypeMoneyline, c.role, domain.NoLine(),
				false, mustScore(t, c.home, c.away))
			if err != nil {
				t.Fatalf("GradeMarket: %v", err)
			}
			if got != c.want {
				t.Errorf("%d-%d on a two-way moneyline %s = %s, want %s",
					c.home, c.away, c.role, got, c.want)
			}
		})
	}
}

// TestGradeMarketMoneylineThreeWay covers the book that DOES quote a draw. The
// only difference from the two-way table is the tie, and it is the whole point:
// grading a three-way tie as a push would refund two bets the book won and
// confiscate one it lost.
func TestGradeMarketMoneylineThreeWay(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		role       domain.SelectionRole
		home, away int
		want       domain.LegStatus
	}{
		{"home backed, home wins", domain.SelectionRoleHome, 2, 1, domain.LegStatusWon},
		{"home backed, away wins", domain.SelectionRoleHome, 1, 2, domain.LegStatusLost},
		{"home backed, drawn — LOSES, it does not push", domain.SelectionRoleHome, 1, 1, domain.LegStatusLost},
		{"away backed, away wins", domain.SelectionRoleAway, 1, 2, domain.LegStatusWon},
		{"away backed, home wins", domain.SelectionRoleAway, 2, 1, domain.LegStatusLost},
		{"away backed, drawn — LOSES", domain.SelectionRoleAway, 1, 1, domain.LegStatusLost},
		{"draw backed, drawn", domain.SelectionRoleDraw, 1, 1, domain.LegStatusWon},
		{"draw backed, goalless draw", domain.SelectionRoleDraw, 0, 0, domain.LegStatusWon},
		{"draw backed, home wins", domain.SelectionRoleDraw, 2, 1, domain.LegStatusLost},
		{"draw backed, away wins", domain.SelectionRoleDraw, 1, 2, domain.LegStatusLost},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := GradeMarket(domain.MarketTypeMoneyline, c.role, domain.NoLine(),
				true, mustScore(t, c.home, c.away))
			if err != nil {
				t.Fatalf("GradeMarket: %v", err)
			}
			if got != c.want {
				t.Errorf("%d-%d on a three-way moneyline %s = %s, want %s",
					c.home, c.away, c.role, got, c.want)
			}
		})
	}
}

// TestGradeMarketDrawRoleIgnoresDrawQuoted pins the reasoning in gradeMoneyline:
// a selection with role `draw` can only exist on a market that quotes one, so
// its own presence settles the question and the flag is irrelevant to it. If
// this ever fails, the flag has leaked into a branch it does not belong in.
func TestGradeMarketDrawRoleIgnoresDrawQuoted(t *testing.T) {
	t.Parallel()

	for _, quoted := range []bool{false, true} {
		got, err := GradeMarket(domain.MarketTypeMoneyline, domain.SelectionRoleDraw,
			domain.NoLine(), quoted, mustScore(t, 1, 1))
		if err != nil {
			t.Fatalf("GradeMarket(drawQuoted=%v): %v", quoted, err)
		}
		if got != domain.LegStatusWon {
			t.Errorf("a drawn game with the draw backed and drawQuoted=%v = %s, want won",
				quoted, got)
		}
	}
}

// -----------------------------------------------------------------------------
// Spread
// -----------------------------------------------------------------------------

// TestGradeMarketSpread walks both sides of one market across the whole
// boundary. The `line` column is ALREADY from the selection's own perspective —
// domain.EffectiveLine inverted it for the away side at placement — so a market
// quoted home -3.5 appears here as home line -3.5 and away line +3.5, and the
// grader must not invert it a second time.
func TestGradeMarketSpread(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		role       domain.SelectionRole
		line       float64
		home, away int
		want       domain.LegStatus
	}{
		// Home -3.5 / away +3.5. Margin +4 covers, margin +3 does not.
		{"home -3.5, wins by 4", domain.SelectionRoleHome, -3.5, 24, 20, domain.LegStatusWon},
		{"home -3.5, wins by 3", domain.SelectionRoleHome, -3.5, 24, 21, domain.LegStatusLost},
		{"away +3.5, loses by 4", domain.SelectionRoleAway, 3.5, 24, 20, domain.LegStatusLost},
		{"away +3.5, loses by 3", domain.SelectionRoleAway, 3.5, 24, 21, domain.LegStatusWon},

		// A whole number: both sides push on the exact margin.
		{"home -3, wins by exactly 3", domain.SelectionRoleHome, -3, 24, 21, domain.LegStatusPush},
		{"away +3, loses by exactly 3", domain.SelectionRoleAway, 3, 24, 21, domain.LegStatusPush},
		{"home -3, wins by 4", domain.SelectionRoleHome, -3, 24, 20, domain.LegStatusWon},
		{"away +3, loses by 2", domain.SelectionRoleAway, 3, 24, 22, domain.LegStatusWon},

		// The underdog side of a big number.
		{"home +7, loses by 6", domain.SelectionRoleHome, 7, 20, 26, domain.LegStatusWon},
		{"home +7, loses by 7", domain.SelectionRoleHome, 7, 20, 27, domain.LegStatusPush},
		{"home +7, loses by 8", domain.SelectionRoleHome, 7, 20, 28, domain.LegStatusLost},
		{"away -7, wins by 8", domain.SelectionRoleAway, -7, 20, 28, domain.LegStatusWon},
		{"away -7, wins by 7", domain.SelectionRoleAway, -7, 20, 27, domain.LegStatusPush},
		{"away -7, wins by 6", domain.SelectionRoleAway, -7, 20, 26, domain.LegStatusLost},

		// A pick'em. The line is 0.0, which is a real traded value and not an
		// absent line, so it grades as a moneyline would — except that the tie
		// pushes on both sides.
		{"home pick'em, wins", domain.SelectionRoleHome, 0, 24, 20, domain.LegStatusWon},
		{"home pick'em, loses", domain.SelectionRoleHome, 0, 20, 24, domain.LegStatusLost},
		{"home pick'em, tied", domain.SelectionRoleHome, 0, 21, 21, domain.LegStatusPush},
		{"away pick'em, wins", domain.SelectionRoleAway, 0, 20, 24, domain.LegStatusWon},
		{"away pick'em, tied", domain.SelectionRoleAway, 0, 21, 21, domain.LegStatusPush},

		// A quarter-point line, which is dyadic and therefore exact — a
		// half-point either side of it must not push.
		{"home -3.25, wins by 4", domain.SelectionRoleHome, -3.25, 24, 20, domain.LegStatusWon},
		{"home -3.25, wins by 3", domain.SelectionRoleHome, -3.25, 24, 21, domain.LegStatusLost},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := GradeMarket(domain.MarketTypeSpread, c.role, mustLine(t, c.line),
				false, mustScore(t, c.home, c.away))
			if err != nil {
				t.Fatalf("GradeMarket: %v", err)
			}
			if got != c.want {
				t.Errorf("%d-%d, %s at %+g = %s, want %s",
					c.home, c.away, c.role, c.line, got, c.want)
			}
		})
	}
}

// TestGradeMarketSpreadSidesAreComplementary is the property the table above
// asserts case by case, stated once as an invariant: on one market, the two
// sides can never both win and can never both lose. Either one wins and the
// other loses, or both push.
//
// It is the check that would catch an inversion the table missed, because a
// grader that inverted the away line as well as the away margin would grade
// BOTH sides as the home side and produce two winners on every game.
func TestGradeMarketSpreadSidesAreComplementary(t *testing.T) {
	t.Parallel()

	// A grid over half-point lines and every plausible margin, run in both
	// directions.
	for tenths := -140; tenths <= 140; tenths += 5 {
		homeLine := float64(tenths) / 10
		for home := 0; home <= 40; home += 3 {
			for away := 0; away <= 40; away += 3 {
				final := mustScore(t, home, away)

				gotHome, err := GradeMarket(domain.MarketTypeSpread, domain.SelectionRoleHome,
					mustLine(t, homeLine), false, final)
				if err != nil {
					t.Fatalf("GradeMarket(home): %v", err)
				}
				gotAway, err := GradeMarket(domain.MarketTypeSpread, domain.SelectionRoleAway,
					mustLine(t, -homeLine), false, final)
				if err != nil {
					t.Fatalf("GradeMarket(away): %v", err)
				}

				ok := (gotHome == domain.LegStatusWon && gotAway == domain.LegStatusLost) ||
					(gotHome == domain.LegStatusLost && gotAway == domain.LegStatusWon) ||
					(gotHome == domain.LegStatusPush && gotAway == domain.LegStatusPush)
				if !ok {
					t.Fatalf("%d-%d at home %+g: home graded %s and away graded %s; "+
						"the two sides of one spread must be complementary",
						home, away, homeLine, gotHome, gotAway)
				}
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Total
// -----------------------------------------------------------------------------

// TestGradeMarketTotal walks both sides of a threshold. Unlike a spread, the
// line is ABSOLUTE — both sides share it and nothing is inverted — so the table
// uses one line value for both roles, which is itself part of what is being
// asserted.
func TestGradeMarketTotal(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		role       domain.SelectionRole
		line       float64
		home, away int
		want       domain.LegStatus
	}{
		{"over 47.5, 48 scored", domain.SelectionRoleOver, 47.5, 24, 24, domain.LegStatusWon},
		{"over 47.5, 47 scored", domain.SelectionRoleOver, 47.5, 24, 23, domain.LegStatusLost},
		{"under 47.5, 47 scored", domain.SelectionRoleUnder, 47.5, 24, 23, domain.LegStatusWon},
		{"under 47.5, 48 scored", domain.SelectionRoleUnder, 47.5, 24, 24, domain.LegStatusLost},

		{"over 47, exactly 47 scored", domain.SelectionRoleOver, 47, 24, 23, domain.LegStatusPush},
		{"under 47, exactly 47 scored", domain.SelectionRoleUnder, 47, 24, 23, domain.LegStatusPush},
		{"over 47, 48 scored", domain.SelectionRoleOver, 47, 24, 24, domain.LegStatusWon},
		{"under 47, 46 scored", domain.SelectionRoleUnder, 47, 24, 22, domain.LegStatusWon},

		// A low soccer total, where a goalless draw is a routine result.
		{"under 2.5, goalless", domain.SelectionRoleUnder, 2.5, 0, 0, domain.LegStatusWon},
		{"over 2.5, goalless", domain.SelectionRoleOver, 2.5, 0, 0, domain.LegStatusLost},
		{"over 2.5, three goals", domain.SelectionRoleOver, 2.5, 2, 1, domain.LegStatusWon},
		{"under 2, two goals", domain.SelectionRoleUnder, 2, 1, 1, domain.LegStatusPush},

		// A quarter-point total.
		{"over 47.25, 48 scored", domain.SelectionRoleOver, 47.25, 24, 24, domain.LegStatusWon},
		{"over 47.25, 47 scored", domain.SelectionRoleOver, 47.25, 24, 23, domain.LegStatusLost},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := GradeMarket(domain.MarketTypeTotal, c.role, mustLine(t, c.line),
				false, mustScore(t, c.home, c.away))
			if err != nil {
				t.Fatalf("GradeMarket: %v", err)
			}
			if got != c.want {
				t.Errorf("total %d at %g, %s = %s, want %s",
					c.home+c.away, c.line, c.role, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// What this feed cannot grade
// -----------------------------------------------------------------------------

// TestGradeMarketVoidsWhatItCannotAnswer pins the honesty rule from grader.go:
// a player prop asks about a statistic this results feed does not carry, and a
// futures market asks about a competition that no single event resolves. Both
// void, whatever the score was, because the only defensible alternative to
// guessing is to pay nobody on nothing.
//
// If a results adapter ever supplies player statistics, THIS TEST IS THE ONE
// THAT SHOULD FAIL FIRST — it is where the decision is recorded, and changing it
// deliberately is the point at which the reasoning gets re-read.
func TestGradeMarketVoidsWhatItCannotAnswer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		typ  domain.MarketType
		role domain.SelectionRole
		line domain.Line
	}{
		{"player prop over", domain.MarketTypePlayerProp, domain.SelectionRoleOver, mustLine(t, 62.5)},
		{"player prop under", domain.MarketTypePlayerProp, domain.SelectionRoleUnder, mustLine(t, 62.5)},
		{"player prop outright", domain.MarketTypePlayerProp, domain.SelectionRoleOutright, domain.NoLine()},
		{"futures outright", domain.MarketTypeFutures, domain.SelectionRoleOutright, domain.NoLine()},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, final := range []domain.Score{
				mustScore(t, 0, 0), mustScore(t, 24, 20), mustScore(t, 20, 24),
			} {
				got, err := GradeMarket(c.typ, c.role, c.line, false, final)
				if err != nil {
					t.Fatalf("GradeMarket: %v", err)
				}
				if got != domain.LegStatusVoid {
					t.Errorf("%s %s on a %s final = %s, want void", c.typ, c.role, final, got)
				}
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Refusals
// -----------------------------------------------------------------------------

// TestGradeMarketRefusesAMissingLine covers the two market types that cannot be
// graded without one. A leg reaching the grader with no line is a plumbing
// fault, and it must be reported as one rather than silently treated as a
// pick'em — which is what defaulting the absent line to zero would do, and which
// would grade a -7 favourite as if it were even.
func TestGradeMarketRefusesAMissingLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ  domain.MarketType
		role domain.SelectionRole
	}{
		{domain.MarketTypeSpread, domain.SelectionRoleHome},
		{domain.MarketTypeSpread, domain.SelectionRoleAway},
		{domain.MarketTypeTotal, domain.SelectionRoleOver},
		{domain.MarketTypeTotal, domain.SelectionRoleUnder},
	}

	for _, c := range cases {
		t.Run(c.typ.String()+" "+c.role.String(), func(t *testing.T) {
			t.Parallel()
			got, err := GradeMarket(c.typ, c.role, domain.NoLine(), false, mustScore(t, 24, 20))
			if !errors.Is(err, domain.ErrLineRequired) {
				t.Errorf("GradeMarket with no line = (%s, %v), want an ErrLineRequired", got, err)
			}
			if !errors.Is(err, ErrUnusableLeg) {
				t.Errorf("error is not classified as an unusable leg: %v", err)
			}
			if got != domain.LegStatusUnknown {
				t.Errorf("a refused grading returned %s; it must return the zero status", got)
			}
		})
	}
}

// TestGradeMarketRefusesARoleTheMarketDoesNotAdmit covers the mis-mapped enum.
// domain.MarketType.AllowsRole is the same matrix migrations/00006 enforces on
// the legs table, so a pair that reaches here having escaped both is a bug worth
// stopping on rather than grading under whichever branch happens to match.
func TestGradeMarketRefusesARoleTheMarketDoesNotAdmit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		typ  domain.MarketType
		role domain.SelectionRole
	}{
		{domain.MarketTypeMoneyline, domain.SelectionRoleOver},
		{domain.MarketTypeMoneyline, domain.SelectionRoleOutright},
		{domain.MarketTypeSpread, domain.SelectionRoleDraw},
		{domain.MarketTypeSpread, domain.SelectionRoleUnder},
		{domain.MarketTypeTotal, domain.SelectionRoleHome},
		{domain.MarketTypeFutures, domain.SelectionRoleHome},
	}

	for _, c := range cases {
		t.Run(c.typ.String()+" "+c.role.String(), func(t *testing.T) {
			t.Parallel()
			_, err := GradeMarket(c.typ, c.role, mustLine(t, 0), false, mustScore(t, 1, 0))
			if !errors.Is(err, domain.ErrRoleNotApplicable) {
				t.Errorf("GradeMarket(%s, %s) = %v, want ErrRoleNotApplicable", c.typ, c.role, err)
			}
		})
	}
}

// TestGradeMarketRefusesUnknownEnums covers the zero values, which carry the
// right Go type but never passed a domain constructor — the exact shape of a
// value assembled from a database row by an adapter that mis-mapped a column.
func TestGradeMarketRefusesUnknownEnums(t *testing.T) {
	t.Parallel()

	if _, err := GradeMarket(domain.MarketTypeUnknown, domain.SelectionRoleHome,
		domain.NoLine(), false, mustScore(t, 1, 0)); !errors.Is(err, domain.ErrUnknownMarketType) {
		t.Errorf("an unknown market type gave %v, want ErrUnknownMarketType", err)
	}
	if _, err := GradeMarket(domain.MarketTypeMoneyline, domain.SelectionRoleUnknown,
		domain.NoLine(), false, mustScore(t, 1, 0)); !errors.Is(err, domain.ErrUnknownSelectionRole) {
		t.Errorf("an unknown selection role gave %v, want ErrUnknownSelectionRole", err)
	}
}

// -----------------------------------------------------------------------------
// Grade: the leg-and-result entry point
// -----------------------------------------------------------------------------

// TestGradeCancelledEventVoidsEveryLeg covers the rule that sits above the
// market rules: with no result there is nothing to grade, whatever the leg
// asked.
func TestGradeCancelledEventVoidsEveryLeg(t *testing.T) {
	t.Parallel()

	refs := []LegRef{
		legRef("evt-1", domain.MarketTypeMoneyline, domain.SelectionRoleHome, domain.NoLine()),
		legRef("evt-1", domain.MarketTypeSpread, domain.SelectionRoleAway, mustLine(t, 3.5)),
		legRef("evt-1", domain.MarketTypeTotal, domain.SelectionRoleOver, mustLine(t, 47.5)),
		legRef("evt-1", domain.MarketTypePlayerProp, domain.SelectionRoleUnder, mustLine(t, 62.5)),
		legRef("evt-1", domain.MarketTypeFutures, domain.SelectionRoleOutright, domain.NoLine()),
	}

	for _, ref := range refs {
		got, err := Grade(ref, cancelledResult("evt-1"))
		if err != nil {
			t.Fatalf("Grade(%s): %v", ref.MarketType, err)
		}
		if got != domain.LegStatusVoid {
			t.Errorf("a cancelled event graded a %s leg as %s, want void", ref.MarketType, got)
		}
	}
}

// TestGradeUsesTheGradingLineNotTheBookedOne is the teaser guarantee, asserted
// at the level that matters: two refs identical but for the line grade
// differently, and the one the grader reads is the one the customer bought.
func TestGradeUsesTheGradingLineNotTheBookedOne(t *testing.T) {
	t.Parallel()

	// A home -3.5 that would lose on a 3-point win, teased to +2.5, which wins.
	booked := legRef("evt-1", domain.MarketTypeSpread, domain.SelectionRoleHome, mustLine(t, -3.5))
	teased := legRef("evt-1", domain.MarketTypeSpread, domain.SelectionRoleHome, mustLine(t, 2.5))
	res := finalResult(t, "evt-1", 24, 21)

	gotBooked, err := Grade(booked, res)
	if err != nil {
		t.Fatalf("Grade(booked): %v", err)
	}
	if gotBooked != domain.LegStatusLost {
		t.Fatalf("the unteased leg graded %s; the fixture is wrong, not the grader", gotBooked)
	}

	gotTeased, err := Grade(teased, res)
	if err != nil {
		t.Fatalf("Grade(teased): %v", err)
	}
	if gotTeased != domain.LegStatusWon {
		t.Errorf("a leg teased to +2.5 graded %s on a 3-point win, want won", gotTeased)
	}
}

// TestGradeRefusesAResultForAnotherEvent covers the plumbing fault that would
// otherwise grade a ticket against somebody else's game.
func TestGradeRefusesAResultForAnotherEvent(t *testing.T) {
	t.Parallel()

	ref := legRef("evt-1", domain.MarketTypeMoneyline, domain.SelectionRoleHome, domain.NoLine())
	_, err := Grade(ref, finalResult(t, "evt-2", 24, 20))
	if !errors.Is(err, domain.ErrMismatchedParent) {
		t.Errorf("Grade across events gave %v, want ErrMismatchedParent", err)
	}
	if !errors.Is(err, ErrUnusableLeg) {
		t.Errorf("error is not classified as an unusable leg: %v", err)
	}
}

// TestGradeRefusesAnUnusableResult covers each way [Result.Validate] can fail.
// Every one of them describes a row the results source should never have
// returned, and grading any of them against a zero-valued score would settle a
// live game 0-0.
func TestGradeRefusesAnUnusableResult(t *testing.T) {
	t.Parallel()

	ref := legRef("evt-1", domain.MarketTypeMoneyline, domain.SelectionRoleHome, domain.NoLine())

	cases := []struct {
		name string
		res  Result
	}{
		{"still live", Result{
			EventID: "evt-1", Status: domain.EventStatusLive, FinalisedAt: finalisedAt,
		}},
		{"ended with no score", Result{
			EventID: "evt-1", Status: domain.EventStatusEnded, FinalisedAt: finalisedAt,
		}},
		{"scored but not ended", Result{
			EventID: "evt-1", Status: domain.EventStatusLive,
			Score: mustScore(t, 10, 7), HasScore: true, FinalisedAt: finalisedAt,
		}},
		{"postponed is not a result", Result{
			EventID: "evt-1", Status: domain.EventStatusPostponed, FinalisedAt: finalisedAt,
		}},
		{"no finalisation instant", Result{
			EventID: "evt-1", Status: domain.EventStatusEnded,
			Score: mustScore(t, 24, 20), HasScore: true,
		}},
		{"malformed event id", Result{
			EventID: "", Status: domain.EventStatusCancelled, FinalisedAt: finalisedAt,
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Grade(ref, c.res); !errors.Is(err, ErrUnusableResult) {
				t.Errorf("Grade(%s) = %v, want ErrUnusableResult", c.name, err)
			}
		})
	}
}

// TestResultValidateAcceptsWhatItShould is the other half of the table above.
// A validation that rejects everything passes the negative tests trivially.
func TestResultValidateAcceptsWhatItShould(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		res  Result
	}{
		{"ended with a score", finalResult(t, "evt-1", 24, 20)},
		{"goalless final", finalResult(t, "evt-1", 0, 0)},
		{"cancelled with no score", cancelledResult("evt-1")},
		{"already settled upstream, still a result", Result{
			EventID: "evt-1", Status: domain.EventStatusSettled,
			Score: mustScore(t, 3, 1), HasScore: true, FinalisedAt: finalisedAt,
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := c.res.Validate(); err != nil {
				t.Errorf("Validate(%s) = %v, want nil", c.name, err)
			}
		})
	}
}

// TestLegRefValidateRefusesMalformedIdentifiers covers the boundary check on the
// other port. The values carry the right Go types and never passed a domain
// constructor, which is exactly what a mis-mapped column produces.
func TestLegRefValidateRefusesMalformedIdentifiers(t *testing.T) {
	t.Parallel()

	good := legRef("evt-1", domain.MarketTypeMoneyline, domain.SelectionRoleHome, domain.NoLine())
	if err := good.Validate(); err != nil {
		t.Fatalf("the fixture itself does not validate: %v", err)
	}

	cases := map[string]func(LegRef) LegRef{
		"empty leg id":     func(l LegRef) LegRef { l.LegID = ""; return l },
		"empty wager id":   func(l LegRef) LegRef { l.WagerID = ""; return l },
		"empty event id":   func(l LegRef) LegRef { l.EventID = ""; return l },
		"illegal charset":  func(l LegRef) LegRef { l.LegID = "leg 1"; return l },
		"unknown market":   func(l LegRef) LegRef { l.MarketType = domain.MarketTypeUnknown; return l },
		"unknown role":     func(l LegRef) LegRef { l.Role = domain.SelectionRoleUnknown; return l },
		"role not allowed": func(l LegRef) LegRef { l.Role = domain.SelectionRoleOver; return l },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := mutate(good).Validate(); err == nil {
				t.Errorf("Validate() accepted a leg ref with %s", name)
			}
		})
	}
}
