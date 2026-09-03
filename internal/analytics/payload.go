// The three signal documents: what a finding IS, on the bus and in the database.
//
// # One type per finding, used by both sinks, on purpose
//
// Each struct below is simultaneously the JSON document published to its
// signals.* topic and the argument to the corresponding [Store] method. That is
// deliberate and it is the property that keeps phase 12 honest: a Flink job that
// reproduces the table rows but not the topic records, or the other way round,
// would have reproduced half the contract. One type means the two sinks cannot
// carry different numbers.
//
// # Every field maps onto a column in migrations/00009
//
// The field order, the names and the units follow the migration column for
// column, because the migration is where the CHECK constraints live and those
// constraints are the executable half of the semantics. Where a constraint
// exists, the comment here says so — a reader deciding whether a value is legal
// should not have to open the SQL to find out that
// `magnitude_probability_points = abs(delta_probability)` is enforced by the
// database rather than merely intended.
//
// # Units are stated on every numeric field, without exception
//
// This package carries four different kinds of number that all look like a
// float64: a PROBABILITY in [0,1], a PROBABILITY DELTA in probability points, a
// PERCENTAGE of stake, and a DECIMAL PRICE. Phase 1 refuses to pick between the
// `expected_value` and `expected_value_percent` spellings for exactly this
// reason ("the ambiguity between them is a routine factor-of-100 error"), and a
// cross-language contract makes the cost of a silent unit mismatch much higher
// than a wrong number on one screen: the Flink job would produce a different
// answer and both sides would look internally consistent.
//
// NO MONEY APPEARS ON ANY SIGNAL. A finding is a statement about prices, and
// prices are floats; the moment a stake or a return is involved the value is
// [domain.Money] in integer minor units and it lives on a wager, not here.
package analytics

import (
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// Document versions and the kafka.Message.Type stamped on each topic.
//
// Versioned independently of kafka.EnvelopeVersion, which versions the frame
// around them, and independently of each other, because the three detectors
// change at different rates. Adding an optional field is not a bump; removing,
// renaming, or changing the meaning or the UNIT of one is.
//
// The type name travels on every record rather than being inferred from the
// topic, matching internal/pricing and internal/ingest/normalizer. On a
// retention-based topic that is less critical than on a compacted one — records
// age out rather than lingering under an old version for ever — but a consumer
// that reads three signals topics should classify them all the same way.
const (
	// SchemaVersion is the version of all three documents below. They are bumped
	// together because they are published by one stage from one input record and
	// a consumer reads them as a set.
	SchemaVersion = 1

	// MessageTypeEV, MessageTypeArbitrage and MessageTypeSteam are the
	// kafka.Message.Type values for signals.ev, signals.arb and signals.steam.
	MessageTypeEV        = "signals.ev.v1"
	MessageTypeArbitrage = "signals.arb.v1"
	MessageTypeSteam     = "signals.steam.v1"
)

// -----------------------------------------------------------------------------
// +EV
// -----------------------------------------------------------------------------

// EVSignal is one book's quote on one selection that beats the sharp reference
// book's no-vig fair value by more than the declared threshold.
//
// It is a QUOTE-LEVEL finding, not a market-level one: the same market can yield
// one signal per (selection, book) pair, and migrations/00009 keys ev_signals on
// (selection_id, book_id, quote_observed_at) precisely because that triple
// identifies one offered price at one provider instant. A market-level "best
// bet" record would collapse the multi-book comparison CLAUDE.md §6 asks for.
//
// Every value field except the thresholds is PROPAGATED from
// [pricing.QuoteAssessment] rather than recomputed here. ev.go argues why at
// length; the short form is that internal/pricing already scored this quote
// against this fair value and a second arithmetic path would be a second answer.
type EVSignal struct {
	// SchemaVersion is [SchemaVersion] as written.
	SchemaVersion int `json:"schema_version"`

	SelectionID domain.SelectionID `json:"selection_id"`
	MarketID    domain.MarketID    `json:"market_id"`

	// MarketType is one of moneyline, spread, total, player_prop, futures. The
	// column is constrained to that set and participates in a composite foreign
	// key to markets(id, type), so a mistyped value is rejected by the database
	// rather than stored.
	MarketType string `json:"market_type"`

	LeagueID domain.LeagueID `json:"league_id"`

	// BookID is the book OFFERING the price. ReferenceBookID is the sharp book
	// the fair value was devigged from (ADR 0006).
	//
	// They may be equal, and migrations/00009 permits it deliberately: a book
	// whose own market is under-round beats its own devigged fair value. Under
	// the default Shin devig that cannot happen, so an equal pair is a signal
	// about the DEVIG rather than about the market — which is exactly why it is
	// storable rather than rejected.
	BookID          domain.BookID `json:"book_id"`
	ReferenceBookID domain.BookID `json:"reference_book_id"`

	// DevigMethod is the method that ACTUALLY produced the fair value, which is
	// not necessarily the one the engine was configured with — internal/pricing
	// records a documented fallback. One of multiplicative, additive, power,
	// shin.
	DevigMethod string `json:"devig_method"`

	// OfferedDecimal is the quoted price and OfferedImplied the probability it
	// implies WITH THE BOOK'S MARGIN STILL IN IT. The pair is carried because
	// Edge is computed against the implied and ExpectedValue against the
	// decimal, and a consumer checking one against the other needs both.
	OfferedDecimal odds.Decimal `json:"offered_decimal"`
	OfferedImplied float64      `json:"offered_implied"`

	// Line is the line THIS BOOK quoted on THIS SELECTION — the selection frame,
	// so an away spread carries the inverted number. Absent for a moneyline or a
	// futures market, and migrations/00009 enforces exactly that per market type.
	//
	// It is the quote's own line rather than the market's consensus line because
	// internal/pricing refuses to score a quote at a moved line at all: a signal
	// therefore always sits at the reference book's line, and this field records
	// which one that was.
	Line domain.Line `json:"line"`

	// FairProbability and FairDecimal are the reference book's no-vig fair value
	// for this selection: the probability, and the price that probability
	// implies.
	FairProbability float64      `json:"fair_probability"`
	FairDecimal     odds.Decimal `json:"fair_decimal"`

	// ExpectedValue is expected profit per unit staked, q·d − 1, and
	// ExpectedValuePercent is the same number ×100. Both are strictly positive
	// on a signal — that is what makes it one — and the database CHECKs both.
	ExpectedValue        float64 `json:"expected_value"`
	ExpectedValuePercent float64 `json:"expected_value_percent"`

	// Edge is q/p − 1 and EdgePercent is it ×100. Algebraically identical to
	// ExpectedValue under p = 1/d, computed by a different route (a division
	// against a multiplication), and carried separately for the reason
	// internal/pricing carries both: a consumer comparing them is comparing the
	// arithmetic against itself.
	Edge        float64 `json:"edge"`
	EdgePercent float64 `json:"edge_percent"`

	// Kelly is the full Kelly stake AS A FRACTION OF BANKROLL and FractionalKelly
	// is it scaled by KellyFraction. Neither is money. FractionalKelly ≤ Kelly is
	// a database CHECK.
	Kelly           float64 `json:"kelly"`
	FractionalKelly float64 `json:"fractional_kelly"`

	// KellyFraction is the multiplier that produced FractionalKelly, in (0, 1].
	//
	// It is stored so the scaling is REPRODUCIBLE: without it, a row holding a
	// full and a fractional stake says nothing about the risk posture that
	// produced the second, and a deployment that changed the multiplier would
	// leave two incomparable populations of rows with no way to separate them.
	KellyFraction float64 `json:"kelly_fraction"`

	// QuoteObservedAt is the PROVIDER's observation instant for this quote — the
	// partition column of the hypertable and part of the replay key.
	// QuoteAgeSeconds is its age at the source record's own anchor, never at a
	// wall clock; see ev.go.
	//
	// QuoteAgeSeconds MAY BE NEGATIVE when a provider's clock runs ahead of ours.
	// It is reported rather than clamped, for the reason [domain.Price.Age]
	// gives: "a monitor can detect the skew instead of silently reporting healthy
	// staleness".
	QuoteObservedAt time.Time `json:"quote_observed_at"`
	QuoteAgeSeconds float64   `json:"quote_age_seconds"`

	// ThresholdEVPercent and MaxQuoteAgeSeconds are the bounds THIS FINDING WAS
	// PRODUCED UNDER, carried on the finding itself.
	//
	// They are not decoration. A consumer that cannot see the threshold cannot
	// tell a 1.2% signal found under a 1% bar from a 1.2% signal found under a
	// 5% bar that has since been lowered, and a stored population of findings
	// spanning a configuration change is otherwise uninterpretable. The database
	// additionally CHECKs ExpectedValuePercent ≥ ThresholdEVPercent, so a row can
	// never claim a bound it does not meet.
	ThresholdEVPercent float64 `json:"threshold_ev_percent"`
	MaxQuoteAgeSeconds float64 `json:"max_quote_age_seconds"`

	// DetectedAt is OUR clock at the moment the detector ran. It is stored and
	// published and is NEVER part of any key — see doc.go on replayability.
	DetectedAt time.Time `json:"detected_at"`
}

// -----------------------------------------------------------------------------
// Arbitrage
// -----------------------------------------------------------------------------

// ArbitrageSignal is one under-round line group: a set of quotes, one per
// outcome of one market at one line, whose implied probabilities sum below 1.
//
// It is SURFACED from [pricing.ArbitrageRef], never re-detected. internal/pricing
// owns the mathematics (and routes the summation through [odds.NewMargin] so two
// call sites cannot disagree about S); this package applies the staleness
// discipline, computes the fingerprint that gives the finding a stable identity,
// and persists it.
type ArbitrageSignal struct {
	SchemaVersion int `json:"schema_version"`

	MarketID   domain.MarketID `json:"market_id"`
	MarketType string          `json:"market_type"`
	LeagueID   domain.LeagueID `json:"league_id"`

	// Line is the line every leg was quoted at, IN THE MARKET'S HOME FRAME.
	//
	// This is the one place in the whole signals family where a line is in the
	// home frame rather than the selection frame; the legs below carry the
	// selection frame. Both are true and they are different facts — the group is
	// identified by one handicap, and each leg quotes its own side of it.
	Line domain.Line `json:"line"`

	// SelectionCount is the market's COMPLETE outcome count, which equals
	// len(Legs) by construction: a group that does not cover every outcome is not
	// an arbitrage, and summing two implied probabilities of a three-way market
	// is the classic way to manufacture a firehose of losing bets.
	SelectionCount int `json:"selection_count"`

	// ImpliedSum is S = Σ 1/d over the legs, strictly below 1 by construction.
	// ReturnFraction is the guaranteed profit per unit of total outlay, (1−S)/S.
	//
	// Booking percentage, overround and vig are exact single-operation functions
	// of S and are deliberately NOT stored: a derived number in a second column
	// is a second number that can disagree with the first.
	ImpliedSum     float64 `json:"implied_sum"`
	ReturnFraction float64 `json:"return_fraction"`

	// DistinctBooks is how many different books the legs span. ONE IS LEGAL AND
	// IS THE STRONGER FINDING: a single book under-rounding its own market has no
	// cross-book staleness explanation available to it at all.
	DistinctBooks int `json:"distinct_books"`

	// ObservedSpreadSeconds is the gap between the oldest and newest leg and
	// OldestLegAgeSeconds is the oldest leg's age at the source record's anchor.
	//
	// BOTH ARE PART OF THE FINDING AND BOTH MUST BE RENDERED WHEREVER IT IS.
	// The phase-4 gate measured 68 live arbitrages across 1,065 records with the
	// leg-age bound binding constantly, which is the same observation every
	// serious treatment of cross-book arbitrage makes: most of it is one book not
	// having moved yet. A consumer that cannot see how stale the legs are cannot
	// tell a real finding from that, so the two numbers travel on the record
	// rather than being trusted to a filter somebody applied upstream.
	//
	// OldestLegAgeSeconds may be negative under provider clock skew.
	ObservedSpreadSeconds float64 `json:"observed_spread_seconds"`
	OldestLegAgeSeconds   float64 `json:"oldest_leg_age_seconds"`

	// ObservedAt is the OLDEST leg's instant, not the newest and not the record's
	// anchor. An opportunity is exactly as fresh as its stalest leg, and it is
	// part of the replay key for that reason.
	ObservedAt time.Time `json:"observed_at"`

	// LegsFingerprint gives the finding a stable identity across recomputations.
	// See [FingerprintLegs] for the exact function, which phase 12 must
	// reproduce byte for byte.
	LegsFingerprint string `json:"legs_fingerprint"`

	// MaxLegAgeSeconds and MaxObservedSpreadSeconds are the staleness bounds THIS
	// finding was produced under. migrations/00009 CHECKs that the finding
	// actually meets them, which makes the discipline a database fact rather than
	// a claim.
	MaxLegAgeSeconds         float64 `json:"max_leg_age_seconds"`
	MaxObservedSpreadSeconds float64 `json:"max_observed_spread_seconds"`

	// DetectedAt is our clock. Never keyed.
	DetectedAt time.Time `json:"detected_at"`

	// Legs are the outcomes, ordered by LegIndex, which is the order
	// internal/pricing produced them in (the market's selection display order).
	Legs []ArbitrageSignalLeg `json:"legs"`
}

// ArbitrageSignalLeg is one outcome of one arbitrage.
//
// The legs are a PART OF their parent rather than evidence that outlives it:
// migrations/00009 cascades the delete, and a recomputation replaces the whole
// set rather than merging into it. A leg that survived its finding would be a
// row nothing could interpret.
type ArbitrageSignalLeg struct {
	// LegIndex is the leg's position in the parent's outcome order, 0-based. It
	// is half of the primary key, so it must be stable across a recomputation of
	// the same finding — which it is, because internal/pricing emits legs in the
	// market's selection order and that order is a property of the market.
	LegIndex int `json:"leg_index"`

	SelectionID domain.SelectionID `json:"selection_id"`

	// Role is the outcome this leg backs: home, away, draw, over, under,
	// outright.
	Role string `json:"role"`

	BookID      domain.BookID `json:"book_id"`
	DecimalOdds odds.Decimal  `json:"decimal_odds"`

	// Line is this leg's own line, IN THE SELECTION FRAME — the away side of a
	// −3.5 group carries +3.5. Contrast [ArbitrageSignal.Line], which is the home
	// frame.
	Line domain.Line `json:"line"`

	// StakeFraction is this leg's share of the total outlay, q_i / S. Staking in
	// these proportions is what equalises the return across outcomes. It is a
	// ratio, never money.
	StakeFraction float64 `json:"stake_fraction"`

	// ObservedAt is this leg's provider instant and AgeSeconds its age at the
	// source record's anchor. AgeSeconds may be negative under clock skew.
	ObservedAt time.Time `json:"observed_at"`
	AgeSeconds float64   `json:"age_seconds"`
}

// -----------------------------------------------------------------------------
// Steam
// -----------------------------------------------------------------------------

// SteamSignal is one correlated line move: a jump one book took first and other
// books followed, measured over a hopping window on implied-probability
// velocity.
//
// It is the ONE genuinely new detector in phase 9. internal/analytics/steam owns
// the mathematics and states every parameter; this type is what a finding looks
// like once it leaves that package.
//
// # Directional, and keyed as such
//
// Steam is DIRECTIONAL and therefore per-SELECTION, not per-market. "The home
// side steamed" and "the away side steamed" are the same market and opposite
// findings, and migrations/00009 keys steam_signals on
// (market_id, selection_id, window_start, window_end) rather than on the market
// alone — keying by market would silently drop half the findings. The BUS key
// stays at the market so that all of one market's signals stay ordered relative
// to one another on one partition; the finer key belongs where rows are stored,
// not where ordering is bought.
type SteamSignal struct {
	SchemaVersion int `json:"schema_version"`

	MarketID    domain.MarketID    `json:"market_id"`
	MarketType  string             `json:"market_type"`
	LeagueID    domain.LeagueID    `json:"league_id"`
	SelectionID domain.SelectionID `json:"selection_id"`

	// WindowStart and WindowEnd bound the hopping window, HALF-OPEN: an
	// observation at exactly WindowEnd belongs to the NEXT window, never to this
	// one. Half-open is not a convention chosen here — it is what makes
	// consecutive windows a partition of the timeline rather than an overlapping
	// cover with a double-counted boundary, and it is what Flink's HOP does.
	//
	// WindowEnd is the hypertable's partition column and the newest of the two,
	// which is why the recency index is on it.
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`

	// WindowSeconds is WindowEnd − WindowStart and HopSeconds is the distance
	// between consecutive window starts, with HopSeconds ≤ WindowSeconds (a hop
	// longer than the window would leave gaps in the cover). Both are stored on
	// the finding because a threshold in probability points per minute means a
	// different thing at a different window length, so a population of findings
	// spanning a re-tuning is otherwise uninterpretable.
	WindowSeconds float64 `json:"window_seconds"`
	HopSeconds    float64 `json:"hop_seconds"`

	// Direction is "shorten" when the selection's implied probability ROSE (the
	// price shortened) and "drift" when it fell. migrations/00009 CHECKs
	// (direction = 'shorten') = (delta_probability > 0), so the two can never
	// disagree.
	Direction string `json:"direction"`

	// DeltaProbability is the lead book's signed change in IMPLIED PROBABILITY
	// POINTS across the window, MagnitudeProbabilityPoints is its absolute value
	// (enforced by the database, and abs is exact in IEEE so the check is safe),
	// and VelocityProbabilityPerMinute is DeltaProbability divided by the window
	// length in minutes.
	//
	// THE UNIT IS PROBABILITY POINTS, NEVER DECIMAL ODDS, and that is not a
	// preference. Decimal odds are nonlinear in probability: a 0.10 decimal move
	// is 0.045 probability points at d = 1.50 and 0.001 at d = 10.00, so a fixed
	// decimal threshold means five different things across one board. steam's
	// package doc argues it at length.
	DeltaProbability             float64 `json:"delta_probability"`
	MagnitudeProbabilityPoints   float64 `json:"magnitude_probability_points"`
	VelocityProbabilityPerMinute float64 `json:"velocity_probability_per_minute"`

	// DevigMethod is "none" for a detector that works on raw implied
	// probabilities, which is what this one does and why the column admits a
	// fifth value the other tables' devig_method columns do not. steam's package
	// doc gives the argument: devigging needs a book's COMPLETE outcome set at
	// one instant, and a book that has only refreshed half its market is exactly
	// the book whose lag carries the signal — dropping it would remove the
	// evidence.
	DevigMethod string `json:"devig_method"`

	// LeadBookID is the book that moved FIRST and LeadMovedAt is the instant it
	// moved, which lies inside [WindowStart, WindowEnd) by construction and is
	// CHECKed to.
	LeadBookID  domain.BookID `json:"lead_book_id"`
	LeadMovedAt time.Time     `json:"lead_moved_at"`

	// Followers are the other books that moved the same way afterwards, ORDERED
	// BY LAG ASCENDING and then by book identifier. The order is part of the
	// contract because the column is JSONB: a database cannot enforce the
	// ordering of an array, so the writer owns it and phase 12 must reproduce it.
	//
	// FollowerCount = len(Followers) and ParticipatingBooks = FollowerCount + 1
	// are both CHECKed against the array, which is what stops a denormalised
	// count from drifting away from the thing it counts.
	Followers          []SteamFollower `json:"followers"`
	FollowerCount      int             `json:"follower_count"`
	ParticipatingBooks int             `json:"participating_books"`

	// CrossBookCorrelation is the mean signed agreement across every book with
	// enough data in the window, in [-1, 1]. See steam's package doc for the
	// exact statistic and — more importantly — for what it does and does not
	// discriminate.
	CrossBookCorrelation float64 `json:"cross_book_correlation"`

	// The four thresholds this finding was produced under. migrations/00009
	// CHECKs that it meets all four, so a stored row cannot claim a bound it
	// fails.
	ThresholdVelocity     float64 `json:"threshold_velocity"`
	ThresholdMagnitude    float64 `json:"threshold_magnitude"`
	ThresholdCorrelation  float64 `json:"threshold_correlation"`
	MinFollowers          int     `json:"min_followers"`
	MaxFollowerLagSeconds float64 `json:"max_follower_lag_seconds"`

	// DetectedAt is our clock. Never keyed.
	DetectedAt time.Time `json:"detected_at"`
}

// SteamFollower is one book that followed the lead.
//
// The JSON field names are the contract: the column is JSONB and
// migrations/00009 can check that the value is an array of the right length but
// cannot check the shape of its elements, so this struct's tags ARE the schema.
// BookID in particular is not foreign-key enforced inside JSONB, which makes it
// the writer's obligation to emit a book the catalogue knows.
type SteamFollower struct {
	BookID domain.BookID `json:"book_id"`

	// MovedAt is the instant this book moved, in RFC 3339 with a UTC offset.
	MovedAt time.Time `json:"moved_at"`

	// LagSeconds is MovedAt − the lead's move instant, in seconds, and is ≥ 0 by
	// construction: a book that moved before the lead would BE the lead.
	LagSeconds float64 `json:"lag_seconds"`

	// DeltaProbability is this book's own signed window change in probability
	// points, same sign as the lead's by construction.
	DeltaProbability float64 `json:"delta_probability"`
}
