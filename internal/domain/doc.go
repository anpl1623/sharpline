// Package domain holds the core language of Sharpline: the entity types that
// every other package speaks in, and the rules that make an instance of one of
// them meaningful.
//
// # The hierarchy
//
// CLAUDE.md §4 fixes the vocabulary:
//
//		Sport → League → Event → Market → Selection → Price
//
//	  - [Sport] is the top of the tree ("basketball").
//	  - [League] is a competition within a sport ("nba").
//	  - [Event] is a contest: two [Competitor]s, a scheduled start, an optional
//	    live [GameClock] and [Score], and an [EventStatus] lifecycle.
//	  - [Market] is a question about an event — moneyline, spread, total, player
//	    prop, futures — carrying a [MarketType] and a [Line] where applicable.
//	  - [Selection] is an answer to that question: a side, an over/under, or a
//	    named outright runner.
//	  - [Price] is the odds for a selection at a [Book] at an instant. It is
//	    immutable; a new quote is a new value, never an edit. This is the type
//	    that becomes the TimescaleDB hypertable.
//
// # Purity
//
// This package performs no I/O and depends on nothing outside the standard
// library (CLAUDE.md §8: "types + odds math — zero external deps"). There is no
// database handle, no HTTP client, no logger, no context plumbing, and — this
// one is easy to violate by accident — no clock read. Nothing here calls
// [time.Now]. Every operation whose answer depends on the current instant takes
// that instant as a parameter, which is what makes the whole package trivially
// testable and deterministic.
//
// There is no package-level mutable state. The status machines are implemented
// as switches rather than maps precisely so that no code can reach in and edit
// a transition table at run time.
//
// # Immutability
//
// Entity types keep their fields unexported and are constructed only through a
// validating constructor. There are no setters. An operation that changes state
// — [Event.WithStatus], [Market.WithLine] — returns a new value and leaves the
// receiver untouched, so a value that has been validated once stays valid for
// its whole life and can be shared across goroutines without a lock.
//
// The cost is that downstream layers cannot build these types with a struct
// literal and cannot marshal them directly. That is deliberate: the HTTP and
// persistence layers should own their own wire and row shapes and map across
// the boundary through the constructors and accessors here. The two exceptions
// that do implement encoding interfaces are [Line] and the enums, because their
// absent-versus-zero and string-versus-integer distinctions have to survive
// serialization or the modelling effort is wasted.
//
// # Money
//
// [Money] is an int64 count of minor units. CLAUDE.md §12: "All money and stake
// values are integer minor units. Floating point never touches a balance. Odds
// and probabilities are floats; ledger amounts are not." Exactly one function
// lets a float64 influence a Money value, [Money.MulFloat], and it demands an
// explicit [Rounding] mode so that the rounding decision is always written down
// at the call site. See money.go for the single-currency argument.
//
// # Odds
//
// Odds are carried as decimal odds in a float64. Decimal is the storage format
// because it is the only one that is total over the useful range: American odds
// are undefined between -100 and +100, and fractional odds need a rational
// rather than a float. Conversion to American, fractional, and implied
// probability, along with devigging, EV, and Kelly, live in
// internal/domain/odds. Deliberately, none of that math is duplicated here —
// two implementations of the same formula will diverge, and CLAUDE.md §10 is
// blunt about what wrong odds math costs.
//
// # Errors
//
// Every failure is a sentinel wrapped with context. There are two roots:
// [ErrInvalid] for a value that could never be correct, and [ErrConflict] for a
// value that is well-formed but illegal against current state. The distinction
// is the one an HTTP layer needs — 400 versus 409 — without this package having
// to know that HTTP exists. Nothing here panics.
package domain
