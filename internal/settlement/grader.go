// Grading: the pure function that decides who gets paid.
//
// Nothing in this file does I/O, reads a clock, or holds state. Given what the
// customer bought and how the game finished, it returns a domain.LegStatus and
// nothing else. That is deliberate and it is the reason the exhaustive tests in
// grader_test.go are worth their length: this is the one computation in the
// system where a sign error is a silent transfer of money, and a pure function
// is the only shape that can be tested to exhaustion.
//
// # It grades the line the customer BOUGHT, never the line the market quoted
//
// [LegRef.GradingLine] is domain.Leg.GradingLine(): the teased line where the
// ticket was teased, the booked line otherwise, and already inverted for an away
// spread by domain.EffectiveLine at placement. This file applies it as given and
// never re-derives it. leg.go states the guarantee that makes that safe — a leg
// "carries everything the grader needs" — and states the bug it prevents: "a leg
// that reads the current line grades correctly in every test where the line
// never moves, and pays the wrong amount exactly when it matters".
//
// # Exact comparison is correct here, and a tolerance would be a defect
//
// A spread and a total both grade by comparing an INTEGER quantity — a margin,
// a combined total — against a line. Books quote lines on a half- or
// quarter-point grid, every value on that grid is a dyadic rational and
// therefore exact in float64, and integers below 2^53 are exact too. So the sum
// and the difference are exact, and equality means the result landed ON the
// number, which is what a push IS.
//
// Introducing a tolerance would not make this more robust; it would make a
// genuine one-point win into a push whenever somebody chose a tolerance larger
// than the grid. The rest of the domain uses relative tolerances for float
// comparisons because it is comparing two computations of one price. This is not
// that: it is asking whether a scoreboard reached a threshold.
package settlement

import (
	"fmt"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Grade decides one pending leg against one result.
//
// It is the entry point settle.go calls, and it is a thin composition of two
// rules over [GradeMarket]:
//
//   - A CANCELLED event voids every leg on it, whatever the market type. There
//     is no result to grade against and there never will be; the stake goes
//     back. Note that a POSTPONED event is not a result at all — [Result] refuses
//     it — because domain.EventStatus admits `postponed → scheduled` and voiding
//     a game that is merely being rescheduled would cancel live bets on a
//     contest that is still going to happen.
//
//   - Otherwise the event was played to a final score and the market's own rule
//     applies.
//
// Both arguments are validated rather than trusted. They arrive from adapters
// over tables this package does not own, and a leg whose role does not match its
// market type would otherwise fall through to a default branch and be graded by
// whichever rule happened to be written last.
func Grade(ref LegRef, res Result) (domain.LegStatus, error) {
	if err := ref.Validate(); err != nil {
		return domain.LegStatusUnknown, err
	}
	if err := res.Validate(); err != nil {
		return domain.LegStatusUnknown, err
	}
	if ref.EventID != res.EventID {
		return domain.LegStatusUnknown, fmt.Errorf("%w: leg %s is on event %s, not %s: %w",
			ErrUnusableLeg, ref.LegID, ref.EventID, res.EventID, domain.ErrMismatchedParent)
	}
	if res.IsCancelled() {
		return domain.LegStatusVoid, nil
	}
	return GradeMarket(ref.MarketType, ref.Role, ref.GradingLine, ref.DrawQuoted, res.Score)
}

// GradeMarket is the grading rule itself: what the leg asks, which side of it
// the customer took, the line it settles at, and the final score.
//
// drawQuoted is the fifth argument and it is the one that is not obvious. It
// reports whether the moneyline market also quotes a draw, and it is the only
// input a leg cannot supply for itself — see [LegRef.DrawQuoted] for why. It is
// ignored for every market type other than moneyline.
func GradeMarket(
	typ domain.MarketType,
	role domain.SelectionRole,
	line domain.Line,
	drawQuoted bool,
	final domain.Score,
) (domain.LegStatus, error) {
	if !typ.Valid() {
		return domain.LegStatusUnknown, fmt.Errorf("%w: %w", ErrUngradableMarket, domain.ErrUnknownMarketType)
	}
	if !role.Valid() {
		return domain.LegStatusUnknown, fmt.Errorf("%w: %w", ErrUnusableLeg, domain.ErrUnknownSelectionRole)
	}
	if !typ.AllowsRole(role) {
		return domain.LegStatusUnknown, fmt.Errorf("%w: %s is not an answer a %s market admits: %w",
			ErrUnusableLeg, role, typ, domain.ErrRoleNotApplicable)
	}

	switch typ {
	case domain.MarketTypeMoneyline:
		return gradeMoneyline(role, drawQuoted, final)
	case domain.MarketTypeSpread:
		return gradeSpread(role, line, final)
	case domain.MarketTypeTotal:
		return gradeTotal(role, line, final)
	case domain.MarketTypePlayerProp:
		return domain.LegStatusVoid, nil
	case domain.MarketTypeFutures:
		return domain.LegStatusVoid, nil
	default:
		// Unreachable while domain.MarketType has five members and Valid()
		// admits exactly those five. It is here because the day a sixth is
		// added, this is the branch that must be seen: an unhandled market type
		// that fell through to a default of "won" or "lost" would pay or
		// confiscate under a rule nobody wrote, and "void everything we do not
		// recognise" would silently cancel a whole product line.
		return domain.LegStatusUnknown, fmt.Errorf("%w: %s", ErrUngradableMarket, typ)
	}
}

// gradeMoneyline decides who won outright.
//
// # The tie, and why it needs a fact the leg does not carry
//
// A two-way moneyline and a three-way moneyline are the same market type, the
// same roles, and completely different rules on a tie. On a two-way book the tie
// is a PUSH — there was no third price to take, so the bet is off and the stake
// comes back. On a three-way book the tie is a real, separately priced outcome:
// home and away both LOSE, and the draw selection wins. Grading a three-way tie
// as a push would refund two bets the book won and confiscate one it lost.
//
// Both shapes are live in this system. The synthetic provider quotes a draw on
// the moneyline for exactly the leagues whose sport admits one and quotes two
// ways elsewhere (internal/ingest/provider/synthetic/markets.go), and
// theoddsapi's mapper maps a draw outcome where the provider sends one. So the
// distinction cannot be assumed away in either direction, and drawQuoted carries
// it in from the catalogue.
//
// A draw-role leg needs no such flag: a selection with role `draw` only exists
// on a market that quotes one, so its own presence settles the question.
func gradeMoneyline(role domain.SelectionRole, drawQuoted bool, final domain.Score) (domain.LegStatus, error) {
	margin := final.Margin()

	switch role {
	case domain.SelectionRoleHome:
		return sideOutcome(margin > 0, margin < 0, drawQuoted), nil
	case domain.SelectionRoleAway:
		return sideOutcome(margin < 0, margin > 0, drawQuoted), nil
	case domain.SelectionRoleDraw:
		if margin == 0 {
			return domain.LegStatusWon, nil
		}
		return domain.LegStatusLost, nil
	default:
		// AllowsRole already rejected everything else; this keeps the switch
		// total so a role added to domain.SelectionRole cannot silently acquire
		// the behaviour of whichever branch fell through.
		return domain.LegStatusUnknown, fmt.Errorf("%w: %s on a moneyline: %w",
			ErrUnusableLeg, role, domain.ErrRoleNotApplicable)
	}
}

// sideOutcome collapses the two-way / three-way tie rule for one side of a
// moneyline. won and lost are the side's own tests; when neither holds the game
// was tied, and the tie is a loss on a three-way book and a push on a two-way
// one.
func sideOutcome(won, lost, drawQuoted bool) domain.LegStatus {
	switch {
	case won:
		return domain.LegStatusWon
	case lost:
		return domain.LegStatusLost
	case drawQuoted:
		return domain.LegStatusLost
	default:
		return domain.LegStatusPush
	}
}

// gradeSpread decides a handicap.
//
// # The away side's sign, which the domain has already normalised
//
// domain.EffectiveLine inverts the market's line for the away selection at
// placement time, and legs.price_line stores the inverted value — so a market at
// home -3.5 gives an away leg a grading line of +3.5. This function therefore
// does NOT invert the line. It inverts the MARGIN, which is signed home-minus-
// away by definition (domain.Score.Margin), so that both sides are evaluated as
// "my points, handicapped, against yours":
//
//	home:  ( margin) + line
//	away:  (−margin) + line
//
// Positive wins, negative loses, exactly zero pushes. Worked through on a market
// at home −3.5, away +3.5, final 24–20 (margin +4):
//
//	home −3.5:  +4 − 3.5 = +0.5  → won
//	away +3.5:  −4 + 3.5 = −0.5  → lost
//
// and on the same market at a final of 24–21 (margin +3):
//
//	home −3.5:  +3 − 3.5 = −0.5  → lost
//	away +3.5:  −3 + 3.5 = +0.5  → won
//
// and on a whole-number market at home −3, final 24–21:
//
//	home −3:    +3 − 3 = 0      → push
//	away +3:    −3 + 3 = 0      → push
//
// Inverting both the line and the margin — the mistake this comment exists to
// prevent — grades the away side against the home side's handicap and produces a
// plausible, wrong answer on every game that is not a pick'em.
func gradeSpread(role domain.SelectionRole, line domain.Line, final domain.Score) (domain.LegStatus, error) {
	value, ok := line.Value()
	if !ok {
		return domain.LegStatusUnknown, fmt.Errorf("%w: a spread leg settles at a line: %w",
			ErrUnusableLeg, domain.ErrLineRequired)
	}

	margin := float64(final.Margin())
	if role == domain.SelectionRoleAway {
		margin = -margin
	}
	return thresholdOutcome(margin + value), nil
}

// gradeTotal decides a threshold on combined scoring.
//
// The line is absolute: both sides share it, and domain.EffectiveLine does not
// invert it. So the comparison is the same quantity for both roles with the
// sense flipped, which is what the single subtraction below expresses — over
// wants the total above the number, under wants it below, and landing on the
// number is a push for both.
func gradeTotal(role domain.SelectionRole, line domain.Line, final domain.Score) (domain.LegStatus, error) {
	value, ok := line.Value()
	if !ok {
		return domain.LegStatusUnknown, fmt.Errorf("%w: a total leg settles at a line: %w",
			ErrUnusableLeg, domain.ErrLineRequired)
	}

	over := float64(final.Total()) - value
	if role == domain.SelectionRoleUnder {
		over = -over
	}
	return thresholdOutcome(over), nil
}

// thresholdOutcome maps a signed distance from the number onto a grading.
// Positive is on the customer's side, negative is on the book's, and exactly
// zero is the number itself — see the file comment for why that equality is
// exact rather than approximate.
func thresholdOutcome(distance float64) domain.LegStatus {
	switch {
	case distance > 0:
		return domain.LegStatusWon
	case distance < 0:
		return domain.LegStatusLost
	default:
		return domain.LegStatusPush
	}
}

// -----------------------------------------------------------------------------
// What this feed can and cannot grade
// -----------------------------------------------------------------------------

// The two market types above that return [domain.LegStatusVoid] unconditionally
// do so as a stated decision, not as an oversight, and the reasoning is here
// rather than inline so it is one paragraph rather than two half-arguments.
//
// # Player props
//
// A player prop asks whether a named individual went over or under a threshold —
// receiving yards, points, strikeouts. The results feed carries [domain.Score],
// which is a team score and nothing else, and no amount of arithmetic over a
// final of 24–20 answers "did this receiver clear 62.5 yards". There are exactly
// three things a grader can do with a question it has no data for:
//
//	guess "won"  → pays out on a fabricated result
//	guess "lost" → confiscates a stake on a fabricated result
//	void         → returns the stake, and pays nobody on nothing
//
// Only the third is defensible, and it is the same call the domain already makes
// for a cancelled market: domain.LegStatusVoid means "the leg is removed from
// the wager", contributes a multiplier of exactly 1 to a parlay, and reprices
// the ticket as though the leg were never added. A customer is made whole and
// the book takes no position on a question it cannot answer.
//
// This is a property of THE FEED, not of the market type. The moment a results
// adapter supplies per-player statistics — the real provider adapter of
// CLAUDE.md §11, or a richer synthetic generator — [Result] grows a field, this
// file grows a rule, and nothing else in the settlement path changes. That is
// the entire reason [ResultsSource] is a port.
//
// # Futures
//
// A futures market asks who wins a COMPETITION: a division, a championship, a
// tournament. Its selections are outright runners and its result is not a
// contest's final score — it is which runner ended the season on top, which is
// a fact about a league over months rather than about any single event.
//
// The results feed here is per-event by construction, so a futures leg has no
// event whose ending resolves it. Voiding is the honest answer for the same
// three-way reason as above. Note also what would happen without this branch: a
// futures leg's grading line is absent (migrations/00006's legs_price_line_rule
// requires NULL for futures), so a spread or total rule applied to one would
// have been refused for a missing line and reported as a plumbing fault — which
// is better than a wrong grading, but worse than saying plainly that this system
// does not settle futures yet.
//
// Settling futures properly needs a competition-level results source and a
// mapping from a runner to a selection. It is a real feature, it is not phase 8,
// and pretending otherwise by grading them against a game score would be the
// worst available outcome.
