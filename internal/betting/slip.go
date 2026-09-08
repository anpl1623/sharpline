// The bet slip: what the customer submitted, what they saw when they submitted
// it, and the two-stage validation that turns it into legs.
//
// # Why validation is in two stages and not one
//
// [Slip.Validate] runs the checks that are decidable WITHOUT ANY I/O: empty,
// too long, duplicate selection, stake positive, kind/leg-count arity, teaser
// points, round-robin sizes, teasability by nothing more than what the customer
// asked for. Those refusals are the same at any instant for any customer, so
// running them before the transaction opens keeps a malformed slip from holding
// a database connection and a row lock while it is rejected.
//
// The rest — the market is open, the event will take a bet, the quote is fresh,
// two legs answer the same market, the price has not moved — is decidable only
// against state read inside the placement transaction, and placement.go raises
// it there. Splitting them the other way (validating everything in the
// transaction) would work; splitting them at all is what makes the pure half
// testable with no fakes, which is most of this file's test suite.
//
// # Price-change detection, and the sentence this file exists for
//
// CLAUDE.md §4: "Legs hold the price at placement time, never a live
// reference." §6 asks for "live price-change detection on the slip with
// accept/reject". Put together, the requirement is that a ticket is never
// booked at a number the customer did not see and agree to.
//
// The mechanism here makes that structural rather than careful:
//
//   - Every [SlipLeg] carries SeenDecimal and SeenLine — what was on screen.
//   - [checkPriceMove] compares them against the quote re-read inside the
//     transaction and returns a *[PriceMove] unless they agree.
//   - The leg that gets booked is built from quote.Price, the store's own
//     [domain.Price] value. The seen numbers are used for the comparison and
//     for nothing else, so there is no expression in this package that turns a
//     request field into a booked price.
//
// # Why an IMPROVED price is refused too, by default
//
// Every real book takes a price that moved in the customer's favour without
// asking. This package does not, unless the slip opts in with
// [Slip.AcceptBetterPrice], and the reason is narrow: "accept when the new
// price is longer" and "accept when the new price is shorter" are one
// comparison operator apart, and the difference between them is invisible in
// review and invisible in every test where the line does not move. leg.go makes
// the same argument about re-resolving a booked leg — "the bug it prevents is
// invisible in review ... and pays the wrong amount exactly when it matters".
//
// So the concession is real but it is EXPLICIT and it is on the slip, where a
// reader can see that the customer asked for it. That is also how books
// actually model it: "accept better odds" is a setting, not a silent policy.
//
// # Why a LINE move is never waved through, at all
//
// AcceptBetterPrice covers a longer price at an UNCHANGED line and nothing
// else. price.go says why: "'-3.5 at 1.91' followed by '-4 at 1.95' is a line
// move, while the same two prices with the line stripped look like a pure odds
// move". A different line is a different bet — a spread of -4 loses games that
// -3.5 wins — so "better" has no meaning across one. A line move always
// requires an explicit [Acceptance] naming both the new price and the new line.
package betting

import (
	"fmt"
	"math"

	"github.com/anpl1623/sharpline/internal/domain"
)

// priceMatchTolerance is the RELATIVE tolerance for "is this the same quote".
//
// Exact equality would be defensible — Price.SameQuoteAs uses it, and argues
// that the question is about bytes rather than about numeric closeness. A
// tolerance is used here instead because the seen value has made a round trip
// through JSON and back, and while float64 survives that exactly today, a
// client that formats to a string and re-parses does not.
//
// The magnitude is safe by an enormous margin. The smallest real move a book
// can quote is one tick, and a tick at even money is 0.01 in decimal — a
// relative difference near 5e-3, which is 5 000 000 times this tolerance. No
// genuine price move can hide underneath it, and no representation artefact can
// exceed it.
const priceMatchTolerance = 1e-9

// MaxIdempotencyKeyLen bounds the Idempotency-Key a placement may carry.
//
// The key is hashed rather than stored, so its charset is unconstrained and its
// length has no schema consequence — this bound exists to keep an unbounded
// header off the hash input and out of the logs. 255 is the conventional header
// value length clients assume.
const MaxIdempotencyKeyLen = 255

// Acceptance is the customer's explicit agreement to a specific re-quote.
//
// Both fields must match the current quote for the acceptance to count. Naming
// the line as well as the price is what makes an acceptance meaningful across a
// line move: "yes, book me at 1.95" is not consent to a different handicap, and
// a book that treated it as consent would be moving the customer's bet.
type Acceptance struct {
	Decimal float64
	Line    domain.Line
}

// SlipLeg is one selection on the slip, at one book, together with the quote
// the customer had on screen.
//
// SeenDecimal and SeenLine are NOT the price that gets booked. They are the
// left-hand side of a comparison and nothing else — see the file header.
type SlipLeg struct {
	SelectionID domain.SelectionID

	// BookID is which book's line the customer took. For an in-house ticket
	// that is the synthetic book (domain.NewSyntheticBook).
	BookID domain.BookID

	// SeenDecimal is the decimal price that was on screen. Decimal, not
	// American or fractional: those are display formats converted at the edge
	// and discarded (price.go), and no value in this package is denominated in
	// either.
	SeenDecimal float64

	// SeenLine is the line that was on screen, from THIS SELECTION'S OWN
	// perspective — already inverted for an away spread, exactly as
	// domain.EffectiveLine stores it. domain.NoLine() for a moneyline or a
	// futures market; a present 0.0 is a traded pick'em, which is a different
	// fact from "no line" and is why this is a domain.Line rather than a
	// *float64.
	SeenLine domain.Line

	// Accept, when set, is the customer's explicit agreement to a re-quote they
	// were shown after the first refusal. It must match the CURRENT quote, not
	// the seen one; an acceptance of a price that has itself since moved raises
	// [ErrPriceMovedNotAccepted].
	Accept *Acceptance
}

// Slip is a submitted bet slip: one ticket, or one round robin that expands
// into several.
type Slip struct {
	// Kind decides the arity rules and how the ticket is priced.
	Kind domain.WagerKind

	Legs []SlipLeg

	// Stake is the amount risked, in minor units. For a round robin it is the
	// stake on EACH generated ticket, not the total — that is how books quote it
	// and how customers think about it (wager.go: "'$5 round robin by 2s' on
	// four selections risks $30, not $5"). [Slip.TotalStake] is the total.
	Stake domain.Money

	// Sizes are the round-robin combination sizes: {2} for "by 2s", {2, 3} for
	// "by 2s and 3s". Required for a round robin, and must be empty for every
	// other kind. The domain sorts and de-duplicates them, so {3, 2, 3} and
	// {2, 3} describe the same slip.
	Sizes []int

	// TeaserPoints is the number of points every leg's line moves by. Required
	// and strictly positive on a teaser, zero on everything else.
	TeaserPoints float64

	// SeenTicketDecimal is the whole-ticket price the customer was shown.
	//
	// Required for a parlay and a teaser, where the ticket price is not the leg
	// price and the customer is quoted a separate number. Optional for a
	// straight, where domain.NewWager already enforces that the ticket price
	// equals the single leg's; when it is supplied on a straight it is checked
	// like any other, which costs one comparison and catches a client that is
	// computing the ticket price itself.
	//
	// Deliberately absent for a round robin: its combinations are priced
	// individually from leg prices the customer has already accepted, so there
	// is no single ticket number to show or agree to.
	SeenTicketDecimal float64

	// AcceptTicketDecimal is the explicit agreement to a re-quoted ticket
	// price, in the same shape as a leg's [Acceptance].
	AcceptTicketDecimal *float64

	// AcceptBetterPrice opts in to booking a LONGER price at an UNCHANGED line
	// without a further round trip. It never covers a shorter price and never
	// covers a line move. See the file header for why this is opt-in rather
	// than the default.
	AcceptBetterPrice bool

	// Rounding is the rule stake × price is collapsed under. There is no
	// default and the zero value is refused: money.go is explicit that "a
	// silent default is how a house edge appears in a ledger that nobody meant
	// to put one in", and the mode is frozen onto the ticket so a later partial
	// void reprices under the rule it was written under.
	Rounding domain.Rounding
}

// TotalStake is what the customer actually risks: the stake for a single
// ticket, or stake × ticket count for a round robin.
//
// It is the figure the balance check and the stake limit are evaluated against,
// and getting it wrong in the generous direction is an unfunded ticket.
//
// It returns an error rather than saturating, because the multiplication is
// [domain.Money.MulInt] and a round robin's ticket count sits inside a binomial
// coefficient — bounded by domain.MaxRoundRobinLegs, but bounded to 1013, not
// to something small.
func (s Slip) TotalStake() (domain.Money, error) {
	n, err := s.TicketCount()
	if err != nil {
		return 0, err
	}
	total, err := s.Stake.MulInt(int64(n))
	if err != nil {
		return 0, fmt.Errorf("betting: total stake for %d ticket(s): %w", n, err)
	}
	return total, nil
}

// TicketCount is how many wagers this slip becomes: 1 for everything except a
// round robin, and the sum of C(n, k) over its sizes for one of those.
//
// The count is computed by [domain.RoundRobin.CombinationCount] via
// [Slip.roundRobinParams], not re-derived here — CLAUDE.md §10's rule about two
// implementations of one formula applies with force to a number that multiplies
// the customer's stake.
func (s Slip) TicketCount() (int, error) {
	if s.Kind != domain.WagerKindRoundRobin {
		return 1, nil
	}
	return s.roundRobinTicketCount()
}

// Validate runs every check that is decidable without reading anything.
//
// See the file header for what is deliberately NOT here and why. The order is
// cheapest-and-most-fundamental first, so a caller reading the error learns the
// most basic thing wrong with the slip rather than an incidental consequence of
// it.
func (s Slip) Validate() error {
	if !s.Kind.Valid() {
		return fmt.Errorf("betting: %w: %w", domain.ErrUnknownWagerKind, ErrInvalidSlip)
	}
	if len(s.Legs) == 0 {
		return fmt.Errorf("betting: %w", ErrSlipEmpty)
	}
	if len(s.Legs) > domain.MaxWagerLegs {
		return fmt.Errorf("betting: %d selections, the maximum is %d: %w",
			len(s.Legs), domain.MaxWagerLegs, ErrTooManyLegs)
	}
	if !s.Rounding.Valid() {
		return fmt.Errorf("betting: %w: %w", domain.ErrUnknownRounding, ErrInvalidSlip)
	}
	if !s.Stake.IsPositive() {
		return fmt.Errorf("betting: stake %s: %w", s.Stake, ErrStakeNotPositive)
	}

	if err := s.validateLegIdentity(); err != nil {
		return err
	}
	if err := s.validateArity(); err != nil {
		return err
	}
	if err := s.validateTeaserPoints(); err != nil {
		return err
	}
	if err := s.validateSizes(); err != nil {
		return err
	}
	if err := s.validateSeenPrices(); err != nil {
		return err
	}

	// The total stake is computed rather than merely bounded: a round robin
	// whose ticket count overflows the multiplication is not a large bet, and
	// discovering that at the balance check would report it as a funding
	// problem.
	if _, err := s.TotalStake(); err != nil {
		return fmt.Errorf("%w: %w", err, ErrInvalidSlip)
	}
	return nil
}

// validateLegIdentity refuses a zero selection or book id and a repeated
// selection.
//
// The duplicate-MARKET rule is its sibling and cannot be checked here: a slip
// names selections, and which market a selection answers is read inside the
// transaction. placement.go raises [ErrDuplicateMarket] there. Both rules are
// kept even though a repeated selection implies a repeated market — the same
// argument migrations/00006 makes for keeping both unique indexes.
func (s Slip) validateLegIdentity() error {
	seen := make(map[domain.SelectionID]struct{}, len(s.Legs))
	for i, leg := range s.Legs {
		if leg.SelectionID.IsZero() {
			return fmt.Errorf("betting: selection %d has no id: %w", i, ErrInvalidSlip)
		}
		if leg.BookID.IsZero() {
			return fmt.Errorf("betting: selection %s names no book: %w", leg.SelectionID, ErrInvalidSlip)
		}
		if _, dup := seen[leg.SelectionID]; dup {
			return fmt.Errorf("betting: selection %s: %w", leg.SelectionID, ErrDuplicateSelection)
		}
		seen[leg.SelectionID] = struct{}{}
	}
	return nil
}

// validateArity restates domain.validateLegCount for the slip.
//
// It is a restatement rather than a delegation because domain.NewWager cannot
// be called yet — there are no legs until the quotes are read — and discovering
// "a straight cannot have three selections" after a transaction has opened, a
// row has been locked and three quotes have been read is a round trip and a
// lock hold for a refusal the request already contained. The rule is short
// enough that the duplication is cheaper than the coupling; the domain remains
// the authority and will refuse the same slip again at construction.
func (s Slip) validateArity() error {
	n := len(s.Legs)
	switch s.Kind {
	case domain.WagerKindStraight:
		if n != 1 {
			return fmt.Errorf("betting: a straight with %d selections: %w", n, ErrLegCountForKind)
		}
	case domain.WagerKindParlay, domain.WagerKindTeaser:
		if n < 2 {
			return fmt.Errorf("betting: a %s with %d selection(s): %w", s.Kind, n, ErrLegCountForKind)
		}
	case domain.WagerKindRoundRobin:
		if n < 2 {
			return fmt.Errorf("betting: a round robin with %d selection(s): %w", n, ErrLegCountForKind)
		}
		// The tighter bound: MaxRoundRobinLegs (10), not MaxWagerLegs (25).
		// wager.go calls this the one bound that is load-bearing rather than
		// cosmetic, because the ticket count is a binomial coefficient and "at
		// 20 it would be a million, which is not a large bet, it is a denial of
		// service against the settlement path".
		if n > domain.MaxRoundRobinLegs {
			return fmt.Errorf("betting: a round robin over %d selections, the maximum is %d: %w",
				n, domain.MaxRoundRobinLegs, ErrLegCountForKind)
		}
	}
	return nil
}

// validateTeaserPoints enforces the biconditional domain.validateTeaser
// enforces: points are required on a teaser and refused on everything else.
func (s Slip) validateTeaserPoints() error {
	isTeaser := s.Kind == domain.WagerKindTeaser
	hasPoints := s.TeaserPoints != 0
	if isTeaser != hasPoints {
		return fmt.Errorf("betting: a %s with teaser points %g: %w", s.Kind, s.TeaserPoints, ErrTeaserPoints)
	}
	if !isTeaser {
		return nil
	}
	if math.IsNaN(s.TeaserPoints) || math.IsInf(s.TeaserPoints, 0) {
		return fmt.Errorf("betting: teaser points %v: %w", s.TeaserPoints, ErrTeaserPoints)
	}
	if s.TeaserPoints <= 0 || s.TeaserPoints > domain.MaxTeaserPoints {
		return fmt.Errorf("betting: teaser points %g is outside (0, %g]: %w",
			s.TeaserPoints, domain.MaxTeaserPoints, ErrTeaserPoints)
	}
	// Every teaser leg must be teasable, and the slip already knows enough to
	// say so for the legs that carry no line at all: a moneyline or futures
	// selection has nothing to move. The market TYPE is not known until the
	// quote is read, so the complete check is placement.go's; this one catches
	// the case the customer can see from the screen they were on.
	for _, leg := range s.Legs {
		if !leg.SeenLine.Present() {
			return fmt.Errorf("betting: teaser leg %s was quoted with no line: %w",
				leg.SelectionID, ErrTeaserMarketType)
		}
	}
	return nil
}

// validateSizes enforces the round-robin combination sizes, and their absence
// on every other kind.
func (s Slip) validateSizes() error {
	if s.Kind != domain.WagerKindRoundRobin {
		if len(s.Sizes) > 0 {
			return fmt.Errorf("betting: a %s naming combination sizes %v: %w", s.Kind, s.Sizes, ErrRoundRobinSizes)
		}
		return nil
	}
	if len(s.Sizes) == 0 {
		return fmt.Errorf("betting: a round robin naming no combination size: %w", ErrRoundRobinSizes)
	}
	for _, k := range s.Sizes {
		if k < 2 || k > len(s.Legs) {
			return fmt.Errorf("betting: combination size %d over %d selections: %w",
				k, len(s.Legs), ErrRoundRobinSizes)
		}
	}
	return nil
}

// validateSeenPrices checks that the numbers the customer claims to have seen
// are numbers a book could have quoted.
//
// This is not the price-move check — that needs the current quote — it is the
// guard that stops a nonsense seen-price from making the move check meaningless.
// A NaN seen price compares unequal to everything, so without this a client
// could send NaN and turn every placement into a price-move refusal; a seen
// price of 1e9 would make an "improvement" test pass against any real quote.
func (s Slip) validateSeenPrices() error {
	for _, leg := range s.Legs {
		if err := validateQuotedDecimal(leg.SeenDecimal); err != nil {
			return fmt.Errorf("betting: selection %s seen price: %w: %w", leg.SelectionID, err, ErrInvalidSlip)
		}
		if leg.Accept != nil {
			if err := validateQuotedDecimal(leg.Accept.Decimal); err != nil {
				return fmt.Errorf("betting: selection %s accepted price: %w: %w",
					leg.SelectionID, err, ErrInvalidSlip)
			}
		}
	}

	// The ticket price is bounded by MaxWagerDecimal rather than by
	// MaxDecimalOdds, and the two differ on purpose: a 20-leg parlay of
	// even-money legs is 2^20, an ordinary ticket that the market-price bound
	// would wrongly reject (wager.go).
	if s.wantsTicketPrice() {
		if s.SeenTicketDecimal == 0 {
			return fmt.Errorf("betting: a %s must state the ticket price the customer saw: %w",
				s.Kind, ErrInvalidSlip)
		}
	}
	if s.SeenTicketDecimal != 0 {
		if err := validateTicketDecimal(s.SeenTicketDecimal); err != nil {
			return fmt.Errorf("betting: seen ticket price: %w: %w", err, ErrInvalidSlip)
		}
	}
	if s.AcceptTicketDecimal != nil {
		if err := validateTicketDecimal(*s.AcceptTicketDecimal); err != nil {
			return fmt.Errorf("betting: accepted ticket price: %w: %w", err, ErrInvalidSlip)
		}
	}
	if s.Kind == domain.WagerKindRoundRobin && (s.SeenTicketDecimal != 0 || s.AcceptTicketDecimal != nil) {
		return fmt.Errorf("betting: a round robin has no single ticket price to quote: %w", ErrInvalidSlip)
	}
	return nil
}

// wantsTicketPrice reports whether the slip must state the whole-ticket price
// the customer saw. See [Slip.SeenTicketDecimal] for the per-kind reasoning.
func (s Slip) wantsTicketPrice() bool {
	return s.Kind == domain.WagerKindParlay || s.Kind == domain.WagerKindTeaser
}

// validateQuotedDecimal bounds a SINGLE MARKET price by the domain's own range.
func validateQuotedDecimal(d float64) error {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return fmt.Errorf("%v: %w", d, domain.ErrOddsNotFinite)
	}
	if d <= domain.MinDecimalOdds || d > domain.MaxDecimalOdds {
		return fmt.Errorf("%v: %w", d, domain.ErrOddsOutOfRange)
	}
	return nil
}

// validateTicketDecimal bounds a WHOLE TICKET price, which is a wider range.
func validateTicketDecimal(d float64) error {
	if math.IsNaN(d) || math.IsInf(d, 0) {
		return fmt.Errorf("%v: %w", d, domain.ErrOddsNotFinite)
	}
	if d <= domain.MinDecimalOdds || d > domain.MaxWagerDecimal {
		return fmt.Errorf("%v: %w", d, domain.ErrWagerOddsOutOfRange)
	}
	return nil
}

// roundRobinTicketCount is the sum of C(n, k) over the slip's DISTINCT
// combination sizes.
//
// The sizes are de-duplicated first, because domain.NewRoundRobin sorts and
// compacts them — "{3, 2, 3} and {2, 3} describe the same round robin" — and
// counting a repeated size twice would double the stake the balance check is
// evaluated against relative to the number of tickets actually written.
func (s Slip) roundRobinTicketCount() (int, error) {
	sizes := make(map[int]struct{}, len(s.Sizes))
	for _, k := range s.Sizes {
		if k < 2 || k > len(s.Legs) {
			return 0, fmt.Errorf("betting: combination size %d over %d selections: %w",
				k, len(s.Legs), ErrRoundRobinSizes)
		}
		sizes[k] = struct{}{}
	}
	if len(sizes) == 0 {
		return 0, fmt.Errorf("betting: a round robin naming no combination size: %w", ErrRoundRobinSizes)
	}
	count := 0
	for k := range sizes {
		count += binomial(len(s.Legs), k)
	}
	return count, nil
}

// binomial is C(n, k), computed multiplicatively so no intermediate exceeds the
// result.
//
// THIS IS A SECOND IMPLEMENTATION OF A FORMULA THE DOMAIN ALREADY HAS, which
// CLAUDE.md §10 is blunt about, so it is worth being precise about why it is
// here anyway. domain.RoundRobin.CombinationCount() is the authority and is the
// number used at placement — but it can only be called on a CONSTRUCTED
// RoundRobin, which needs legs, which need quotes read inside the transaction.
// [Slip.Validate] runs before any of that, and its job includes answering
// "would this slip's ticket count overflow the stake multiplication" before a
// connection is checked out and a row is locked.
//
// The duplication is made safe two ways rather than tolerated. placement.go
// compares len(RoundRobin.Combinations()) against the number of derived ids and
// refuses a mismatch outright, so a divergence cannot reach a customer's
// balance. And TestSlipTicketCountAgreesWithTheDomain asserts the two agree for
// every (n, k) the domain admits.
//
// n is bounded by domain.MaxRoundRobinLegs (10), so the largest value this can
// return is C(10,5) = 252 and the intermediate products cannot overflow an int.
func binomial(n, k int) int {
	if k < 0 || k > n {
		return 0
	}
	if k > n-k {
		k = n - k
	}
	result := 1
	for i := 0; i < k; i++ {
		result = result * (n - i) / (i + 1)
	}
	return result
}

// checkPriceMove compares what the customer saw against what the book is
// offering now, for one leg.
//
// It returns nil when the ticket may be booked at `current`, and a *[PriceMove]
// otherwise. The rules, in the order they are applied:
//
//  1. Same price and same line → book it. The overwhelmingly common case.
//  2. An [Acceptance] naming exactly the current price AND the current line →
//     book it. This is the accept/reject round trip of CLAUDE.md §6.
//  3. AcceptBetterPrice, the line unchanged, and the price strictly longer →
//     book it.
//  4. Anything else → refuse.
//
// Note what rule 3 does not do: it never fires when the line moved, however
// favourable the new line looks. See the file header.
func checkPriceMove(leg SlipLeg, current domain.Price, acceptBetter bool) *PriceMove {
	lineMoved := !leg.SeenLine.Equal(current.Line())
	priceSame := floatsMatch(leg.SeenDecimal, current.Decimal())

	if !lineMoved && priceSame {
		return nil
	}

	move := &PriceMove{
		SelectionID:    leg.SelectionID.String(),
		BookID:         leg.BookID.String(),
		SeenDecimal:    leg.SeenDecimal,
		SeenLine:       leg.SeenLine.String(),
		CurrentDecimal: current.Decimal(),
		CurrentLine:    current.Line().String(),
		Improved:       !lineMoved && current.Decimal() > leg.SeenDecimal,
	}

	if leg.Accept != nil {
		if floatsMatch(leg.Accept.Decimal, current.Decimal()) && leg.Accept.Line.Equal(current.Line()) {
			return nil
		}
		// The customer accepted a re-quote and the market moved again while
		// they were reading it. A different sentinel, because the interface
		// this earns is different: they are being told the number they just
		// agreed to is already gone.
		move.Accepted = true
		return move
	}

	if acceptBetter && move.Improved {
		return nil
	}
	return move
}

// checkTicketPriceMove is [checkPriceMove] for the whole-ticket price.
//
// The ticket price has no line, so rule 3 reduces to "the ticket price got
// longer" — which for a parlay is exactly what a leg price improving produces,
// and is therefore the same concession applied consistently.
//
// A slip with no SeenTicketDecimal (a straight that did not state one, a round
// robin that cannot) is not checked here at all: for a straight the domain
// enforces ticket price == leg price, and the leg price was already checked.
func checkTicketPriceMove(s Slip, current float64) *PriceMove {
	if s.SeenTicketDecimal == 0 {
		return nil
	}
	if floatsMatch(s.SeenTicketDecimal, current) {
		return nil
	}

	move := &PriceMove{
		SelectionID:    "",
		BookID:         "",
		SeenDecimal:    s.SeenTicketDecimal,
		SeenLine:       domain.NoLine().String(),
		CurrentDecimal: current,
		CurrentLine:    domain.NoLine().String(),
		Improved:       current > s.SeenTicketDecimal,
	}

	if s.AcceptTicketDecimal != nil {
		if floatsMatch(*s.AcceptTicketDecimal, current) {
			return nil
		}
		move.Accepted = true
		return move
	}

	if s.AcceptBetterPrice && move.Improved {
		return nil
	}
	return move
}

// floatsMatch is "the same quote" under [priceMatchTolerance], relative.
//
// Relative rather than absolute because the quantity is a PRICE, which spans
// four orders of magnitude between an even-money side and a long futures
// price — the opposite of the teased-line comparison in the domain, which is a
// DIFFERENCE of two lines and is therefore absolute. Getting that distinction
// backwards is how a tolerance either stops catching moves on long prices or
// starts rejecting equal ones on short.
//
// NaN is never equal to anything here, including itself: NaN > 0 is false, so
// the comparison below is false and the caller sees a move. [Slip.Validate]
// refuses a NaN seen price before this is ever reached, so that is belt and
// braces rather than the primary defence.
func floatsMatch(a, b float64) bool {
	if a == b {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	if scale == 0 {
		return false
	}
	return math.Abs(a-b)/scale <= priceMatchTolerance
}

// teasedLine returns the line a teaser leg grades at: the booked line moved by
// `points` IN THE BETTOR'S FAVOUR.
//
// The direction rule, and why it is written out here rather than delegated:
//
//	role = over    the threshold moves DOWN   teased = line − points
//	home, away     points are added to your side
//	role = under   the threshold moves UP     teased = line + points
//
// price_line is already stated from the SELECTION's own perspective —
// domain.EffectiveLine inverts the away spread before the price is written — so
// "in the bettor's favour" is "add points" for every role but over, with no
// per-side special case.
//
// THE DOMAIN CANNOT CHECK THIS AND THE DATABASE CAN. domain.validateTeaser
// tests |teased − booked| against the promised points, which is symmetric in
// the sign of the difference: a home spread of −3.5 teased to −9.5, six points
// AGAINST the customer, satisfies it exactly. migrations/00006 found that by
// attacking the database and installed a direction check in
// wagers_assert_shape() — so a wrong-direction tease constructed here would
// pass domain.NewWager and fail at COMMIT. This function is the reason it never
// gets that far, and the reason it is a function rather than three lines inline
// is that "the same magnitude with the wrong sign" is, as that migration puts
// it, "the more dangerous error".
func teasedLine(role domain.SelectionRole, booked domain.Line, points float64) (domain.Line, error) {
	value, ok := booked.Value()
	if !ok {
		return domain.NoLine(), fmt.Errorf("betting: a teased leg's price carries no line: %w", ErrTeaserMarketType)
	}
	if role == domain.SelectionRoleOver {
		return domain.NewLine(value - points)
	}
	return domain.NewLine(value + points)
}
