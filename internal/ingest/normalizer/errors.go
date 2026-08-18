package normalizer

import "errors"

// Sentinel errors. CLAUDE.md §12 puts sentinels in the domain package for domain
// concepts; these are transport- and adapter-level failures that the domain has
// no opinion about, so they live with the code that returns them.
var (
	// ErrInvalidOptions is returned by every constructor in this package when
	// its options do not validate. Configuration fails at construction, loudly,
	// rather than at the first record.
	ErrInvalidOptions = errors.New("normalizer: invalid options")

	// ErrDecode means a provider payload could not be parsed as that provider's
	// documented shape.
	ErrDecode = errors.New("normalizer: decode provider payload")

	// ErrUnsupportedProvider means no decoder is registered for the provider a
	// record arrived from. It is a wiring error, not a data error.
	ErrUnsupportedProvider = errors.New("normalizer: no decoder for provider")

	// ErrNoObservationTime means the payload carried no provider observation
	// instant.
	//
	// It is fatal to the record rather than recoverable, and that is deliberate.
	// The observation instant is the subtrahend in the headline staleness SLO
	// (CLAUDE.md §9). Substituting time.Now() would make every such record
	// report perfect freshness — the metric would look best exactly when the
	// data is least trustworthy — so the record is rejected and counted instead.
	ErrNoObservationTime = errors.New("normalizer: payload carries no observation instant")

	// ErrNothingNormalized means an event payload yielded no publishable market.
	// The per-market reasons are on Result.Rejects.
	ErrNothingNormalized = errors.New("normalizer: payload yielded no market")

	// ErrStaleObservation means a payload was observed strictly before the state
	// already published for that market. Applying it would regress the compacted
	// snapshot, so it is skipped and counted.
	ErrStaleObservation = errors.New("normalizer: observation older than published state")
)

// Scope names the level of the payload a rejection happened at. It is a
// Prometheus label value, so the set is closed.
type Scope string

// The scopes. Each corresponds to one nesting level of a provider payload.
const (
	ScopeEvent     Scope = "event"
	ScopeMarket    Scope = "market"
	ScopeSelection Scope = "selection"
	ScopePrice     Scope = "price"
)

// Reason is a bounded classification of why something was rejected.
//
// It is NEVER the error text. A provider payload is untrusted input and an error
// string built from it can carry arbitrary bytes; using one as a Prometheus label
// value is a cardinality bomb whose author is a third party. The mapping from
// error to Reason is exhaustive and lives at the call site that knows what it was
// attempting, not in a switch over error strings.
type Reason string

// The rejection reasons. Adding one is fine; deriving one from provider data is
// not.
const (
	// ReasonDecode — the payload is not valid JSON for the provider's shape.
	ReasonDecode Reason = "decode"

	// ReasonMissingEventID — no provider event identifier, so nothing can be
	// keyed stably.
	ReasonMissingEventID Reason = "missing_event_id"

	// ReasonMissingLeague — no league/sport key, so the event cannot be placed
	// in the catalogue.
	ReasonMissingLeague Reason = "missing_league"

	// ReasonMissingCompetitor — a match event without both sides. The
	// home-perspective line convention has no meaning without them.
	ReasonMissingCompetitor Reason = "missing_competitor"

	// ReasonMissingStartTime — no commence time, which domain.NewEvent requires.
	ReasonMissingStartTime Reason = "missing_start_time"

	// ReasonNoObservationTime — no last_update at any nesting level.
	ReasonNoObservationTime Reason = "no_observation_time"

	// ReasonInvalidIdentifier — a provider identifier that no derivation can
	// turn into a valid domain identifier.
	ReasonInvalidIdentifier Reason = "invalid_identifier"

	// ReasonInvalidEvent — domain.NewEvent refused the mapped values.
	ReasonInvalidEvent Reason = "invalid_event"

	// ReasonUnsupportedMarket — a market key this build does not map. Expected
	// and frequent: The Odds API publishes hundreds of keys and the charter's
	// board is moneyline, spread and total. It is counted, not logged per
	// occurrence.
	ReasonUnsupportedMarket Reason = "unsupported_market"

	// ReasonMissingSubject — a player-prop outcome carrying no description.
	// The description is the only thing that names the individual, and
	// domain.NewMarket requires a subject on that market type.
	ReasonMissingSubject Reason = "missing_subject"

	// ReasonUnsupportedMessage — a record on odds.raw.{provider} whose envelope
	// type this build does not read. It is a FRAME problem, not a payload
	// problem: internal/ingest's RawMessageType says a bump means the data
	// stopped being the provider's verbatim bytes, so decoding it as though it
	// were would misparse rather than fail.
	ReasonUnsupportedMessage Reason = "unsupported_message"

	// ReasonMissingLine — a spread or total without a point, which
	// domain.MarketType.LineRule reports as required.
	ReasonMissingLine Reason = "missing_line"

	// ReasonInvalidLine — a point that is not finite, or a non-positive total.
	ReasonInvalidLine Reason = "invalid_line"

	// ReasonInvalidMarket — domain.NewMarket refused the mapped values.
	ReasonInvalidMarket Reason = "invalid_market"

	// ReasonNoPrices — a market that produced no usable price. Publishing it
	// would put an empty market on the board.
	ReasonNoPrices Reason = "no_prices"

	// ReasonUnknownRole — an outcome whose name matches neither competitor nor
	// any recognised over/under spelling.
	ReasonUnknownRole Reason = "unknown_role"

	// ReasonDuplicateSelection — the same book quoted the same selection twice
	// in one market. The first is kept.
	ReasonDuplicateSelection Reason = "duplicate_selection"

	// ReasonInvalidSelection — domain.NewSelection refused the mapped values.
	ReasonInvalidSelection Reason = "invalid_selection"

	// ReasonInvalidOdds — a quote that is not a finite decimal price, or an
	// American integer outside the legal band.
	ReasonInvalidOdds Reason = "invalid_odds"

	// ReasonInvalidPrice — domain.NewPrice refused the mapped values.
	ReasonInvalidPrice Reason = "invalid_price"

	// ReasonInvalidBook — domain.NewBook refused the mapped values.
	ReasonInvalidBook Reason = "invalid_book"
)

// Scopes returns every scope, so a metric can be pre-initialised and a test can
// enumerate the closed set rather than restating it.
func Scopes() []Scope {
	return []Scope{ScopeEvent, ScopeMarket, ScopeSelection, ScopePrice}
}

// Reasons returns every rejection reason.
//
// It exists for the same two callers: metrics.go pre-creates no series from it
// (a zero-valued counter for a reason that never fires is noise on a dashboard),
// but errors_test.go walks it to assert the vocabulary is closed, unique, and
// exercised — the phase brief requires a table-driven case for every reason, and
// a list the test derives from the source is the only version of that which
// cannot silently fall behind.
func Reasons() []Reason {
	return []Reason{
		ReasonDecode,
		ReasonMissingEventID,
		ReasonMissingLeague,
		ReasonMissingCompetitor,
		ReasonMissingStartTime,
		ReasonNoObservationTime,
		ReasonInvalidIdentifier,
		ReasonInvalidEvent,
		ReasonUnsupportedMarket,
		ReasonMissingSubject,
		ReasonUnsupportedMessage,
		ReasonMissingLine,
		ReasonInvalidLine,
		ReasonInvalidMarket,
		ReasonNoPrices,
		ReasonUnknownRole,
		ReasonDuplicateSelection,
		ReasonInvalidSelection,
		ReasonInvalidOdds,
		ReasonInvalidPrice,
		ReasonInvalidBook,
	}
}

// Reject is one thing that could not be normalised, and why.
//
// CLAUDE.md's phase brief is explicit that a payload which cannot be normalised
// "must be REJECTED AND COUNTED, never silently coerced into something the domain
// accepts". This type is the counting half; observeReject is the metric half.
type Reject struct {
	// Scope is the payload level the rejection happened at.
	Scope Scope

	// Reason is the bounded classification. It is the Prometheus label value.
	Reason Reason

	// Key identifies what was rejected, in the provider's own vocabulary: a
	// market key, a bookmaker key, an outcome name. TRUNCATED — it is untrusted
	// text and it reaches a log line.
	Key string

	// Err is the underlying error, for the log line. Never a metric label.
	Err error
}

// Error implements error so a Reject can be returned or wrapped directly.
func (r Reject) Error() string {
	if r.Err == nil {
		return string(r.Scope) + " " + string(r.Reason)
	}
	return string(r.Scope) + " " + string(r.Reason) + ": " + r.Err.Error()
}

// Unwrap exposes the underlying error to errors.Is and errors.As.
func (r Reject) Unwrap() error { return r.Err }

// maxKeySample bounds how much of an untrusted provider string reaches a Reject.
// internal/domain/ids.go applies the same idea for the same reason: "provider
// payloads are untrusted; echoing an unbounded string into a log line is how a
// log becomes an attack surface."
const maxKeySample = 48

// sample truncates untrusted text for inclusion in a Reject.
func sample(s string) string {
	if len(s) <= maxKeySample {
		return s
	}
	return s[:maxKeySample] + "…"
}

// reject builds a Reject with the key already truncated.
func reject(scope Scope, reason Reason, key string, err error) Reject {
	return Reject{Scope: scope, Reason: reason, Key: sample(key), Err: err}
}
