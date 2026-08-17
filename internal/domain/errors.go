package domain

import (
	"errors"
	"fmt"
)

// The two roots of the package's error taxonomy.
//
// Every sentinel below wraps exactly one of them, and every error this package
// returns wraps a sentinel. That gives callers three useful granularities:
//
//	errors.Is(err, domain.ErrInvalid)     // "the caller sent nonsense"   → 400
//	errors.Is(err, domain.ErrConflict)    // "not legal from this state"  → 409
//	errors.Is(err, domain.ErrLineRequired) // the precise cause
//
// The split is what an HTTP layer actually needs, expressed without this
// package knowing that HTTP exists (CLAUDE.md §8: zero external deps).
var (
	// ErrInvalid is the root of every validation failure: a value that could
	// not be correct under any circumstances.
	ErrInvalid = errors.New("domain: invalid value")

	// ErrConflict is the root of every state failure: a well-formed value that
	// is not legal against the state it is being applied to. An illegal status
	// transition and an out-of-order update are conflicts, not bad input.
	ErrConflict = errors.New("domain: conflicting state")
)

// Identifier, slug, and display-name failures.
var (
	ErrEmptyID     = fmt.Errorf("%w: identifier is empty", ErrInvalid)
	ErrIDTooLong   = fmt.Errorf("%w: identifier is longer than %d bytes", ErrInvalid, MaxIDLen)
	ErrIDCharset   = fmt.Errorf("%w: identifier contains a byte outside [A-Za-z0-9._-]", ErrInvalid)
	ErrEmptySlug   = fmt.Errorf("%w: slug is empty", ErrInvalid)
	ErrSlugTooLong = fmt.Errorf("%w: slug is longer than %d bytes", ErrInvalid, MaxSlugLen)
	ErrSlugCharset = fmt.Errorf("%w: slug must be [a-z0-9] followed by [a-z0-9_-]", ErrInvalid)
	ErrEmptyName   = fmt.Errorf("%w: display name is empty", ErrInvalid)
	ErrNameTooLong = fmt.Errorf("%w: display name is longer than %d runes", ErrInvalid, MaxNameLen)
	ErrNameCharset = fmt.Errorf("%w: display name contains a control character or invalid UTF-8", ErrInvalid)
)

// Money failures.
var (
	// ErrMoneyOverflow reports arithmetic that would leave the exactly
	// representable range. It is deliberately an ErrInvalid rather than a
	// separate root: a stake large enough to overflow is bad input.
	ErrMoneyOverflow = fmt.Errorf("%w: money arithmetic leaves the safe int64 minor-unit range", ErrInvalid)

	// ErrMoneyPrecision reports a literal carrying more precision than one
	// minor unit. Parsing rejects it rather than silently rounding, because a
	// financial parser that quietly discards a digit is a defect generator.
	ErrMoneyPrecision = fmt.Errorf("%w: money literal has more precision than one minor unit", ErrInvalid)

	ErrMoneySyntax       = fmt.Errorf("%w: money literal is not a signed decimal amount", ErrInvalid)
	ErrMoneyNotFinite    = fmt.Errorf("%w: multiplication factor is NaN or infinite", ErrInvalid)
	ErrMoneyDivideByZero = fmt.Errorf("%w: money division by zero", ErrInvalid)
	ErrUnknownRounding   = fmt.Errorf("%w: rounding mode is not one of the defined modes", ErrInvalid)
)

// Time failures.
var (
	ErrZeroTime = fmt.Errorf("%w: timestamp is the zero time", ErrInvalid)

	// ErrStaleUpdate reports an update stamped earlier than the state it would
	// replace. Kafka delivers at least once and not always in order, so this is
	// an expected, routine outcome that consumers should drop rather than log
	// as an error.
	ErrStaleUpdate = fmt.Errorf("%w: update is older than the state it would replace", ErrConflict)
)

// Enumeration and lifecycle failures.
var (
	ErrUnknownEventKind     = fmt.Errorf("%w: not a defined event kind", ErrInvalid)
	ErrUnknownEventStatus   = fmt.Errorf("%w: not a defined event status", ErrInvalid)
	ErrUnknownMarketType    = fmt.Errorf("%w: not a defined market type", ErrInvalid)
	ErrUnknownMarketStatus  = fmt.Errorf("%w: not a defined market status", ErrInvalid)
	ErrUnknownSelectionRole = fmt.Errorf("%w: not a defined selection role", ErrInvalid)
	ErrUnknownBookKind      = fmt.Errorf("%w: not a defined book kind", ErrInvalid)
	ErrIllegalTransition    = fmt.Errorf("%w: illegal status transition", ErrConflict)
)

// Event failures.
var (
	ErrCompetitorsRequired      = fmt.Errorf("%w: a match event needs both a home and an away competitor", ErrInvalid)
	ErrCompetitorsNotApplicable = fmt.Errorf("%w: an outright event has no home or away competitor", ErrInvalid)
	ErrNegativeScore            = fmt.Errorf("%w: a score cannot be negative", ErrInvalid)
	ErrInvalidPeriod            = fmt.Errorf("%w: a clock period is 1-based and at most %d", ErrInvalid, MaxClockPeriod)
	ErrInvalidElapsed           = fmt.Errorf("%w: elapsed period time is negative or longer than %s", ErrInvalid, MaxClockElapsed)
	ErrClockNotInPlay           = fmt.Errorf("%w: only an in-play event carries a clock", ErrConflict)
	ErrScoreNotApplicable       = fmt.Errorf("%w: only an event that has started carries a score", ErrConflict)
)

// Market and selection failures.
var (
	ErrLineRequired         = fmt.Errorf("%w: this market type requires a line", ErrInvalid)
	ErrLineNotApplicable    = fmt.Errorf("%w: this market type does not take a line", ErrInvalid)
	ErrLineNotFinite        = fmt.Errorf("%w: line is NaN or infinite", ErrInvalid)
	ErrLineNotPositive      = fmt.Errorf("%w: a total's line must be greater than zero", ErrInvalid)
	ErrLineSyntax           = fmt.Errorf("%w: line is not a JSON number or null", ErrInvalid)
	ErrSubjectRequired      = fmt.Errorf("%w: a player-prop market names the player it is about", ErrInvalid)
	ErrSubjectNotApplicable = fmt.Errorf("%w: only a player-prop market carries a subject", ErrInvalid)
	ErrRoleNotApplicable    = fmt.Errorf("%w: selection role is not an answer this market type admits", ErrInvalid)
	ErrMismatchedParent     = fmt.Errorf("%w: child does not belong to the given parent", ErrInvalid)
)

// Price failures.
var (
	ErrOddsNotFinite  = fmt.Errorf("%w: decimal odds are NaN or infinite", ErrInvalid)
	ErrOddsOutOfRange = fmt.Errorf("%w: decimal odds must be greater than %g and at most %g", ErrInvalid, MinDecimalOdds, MaxDecimalOdds)
	ErrLineMismatch   = fmt.Errorf("%w: the price was quoted at a different line than the market carries", ErrConflict)
)
