package pricing

import "errors"

// Sentinel errors. CLAUDE.md §12 puts domain sentinels in the domain package;
// these describe why a market could not be priced, which is this package's
// concern and not the domain's, so they live here. Match with errors.Is, never
// on message text.
var (
	// ErrInvalidOptions is returned by every constructor here when its options
	// do not validate. Configuration fails at construction, loudly, rather than
	// at the first record.
	ErrInvalidOptions = errors.New("pricing: invalid options")

	// ErrNoReferenceBook means no book eligible to be the sharp reference quoted
	// this market.
	//
	// It is a refusal, not a fallback. See doc.go: fair value derived from a
	// consensus is a different quantity from fair value derived from a sharp
	// book, and silently swapping one for the other when the reference is
	// missing makes every downstream EV number unattributable.
	ErrNoReferenceBook = errors.New("pricing: no eligible reference book quoted this market")

	// ErrReferenceStale means the reference book quoted this market, but its
	// most recent observation is older than MaxReferenceAge.
	ErrReferenceStale = errors.New("pricing: reference book quote is stale")

	// ErrIncompleteReference means the reference book did not quote every
	// selection on the market.
	//
	// Devigging is a statement about a COMPLETE market: the margin is the excess
	// of Σ 1/d over 1, and a partial market has no such excess to remove. A
	// two-way market missing one side sums to well under 1 and would devig to a
	// fabricated near-certainty.
	ErrIncompleteReference = errors.New("pricing: reference book did not quote every selection")

	// ErrMarketNotPriceable means the market itself cannot carry a fair value —
	// fewer than odds.MinMarketSelections selections, or no prices at all.
	ErrMarketNotPriceable = errors.New("pricing: market cannot be priced")

	// ErrDevigFailed means every configured devig method, including the
	// fallback, refused the reference book's prices.
	ErrDevigFailed = errors.New("pricing: devig failed")

	// ErrStaleRecord means a normalized record was observed strictly before the
	// state already applied for that market. Applying it would regress the
	// pricer's view and republish an older computed price onto a compacted
	// topic; it is skipped and counted. Same guard, same reason, as the
	// normalizer's.
	ErrStaleRecord = errors.New("pricing: record older than applied state")

	// ErrUnsupportedMessage means a record on odds.normalized carried a message
	// type this build does not read. A wiring error, not a data error.
	ErrUnsupportedMessage = errors.New("pricing: unsupported message type")
)
