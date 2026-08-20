// The record this package publishes to signals.clv.
//
// # It is self-contained, and it is a REFERENCE OUTPUT
//
// CLAUDE.md §3 names signals.clv in the phase-9 event flow and §11 makes phase 9
// "the reference implementation phase 12 validates against". This document is
// where that validation actually happens: the Flink SQL job is run over the same
// Kafka topics, its output is compared against these records, and "same inputs,
// same outputs, or the Flink job is wrong."
//
// That is why the record carries more than the numbers. It carries the two
// PARAMETERS OF THE DEFINITION — the closing lookback and the taken lookback —
// and the devig method that actually produced the two fair probabilities. A
// consumer holding a record with a CLV of +2.1% and nothing else cannot tell
// whether a reimplementation that says +1.8% disagrees about the arithmetic or
// about which quote was the close. With the parameters on the record, it can.
//
// The same discipline as the three signal tables in migrations/00009, which store
// every threshold beside every finding, and for the same stated reason: a finding
// that does not carry the rule it was found under cannot be reproduced.
//
// # Money does not appear here at all
//
// Deliberately. CLAUDE.md §12 puts every money value in integer minor units, and
// the cleanest way to honour that on a record about PRICES is to carry no money
// on it. Closing line value is a property of the price and not of the stake —
// odds/clv.go: "unweighted because CLV is a property of the price, not of the
// stake, and stake-weighting would let a bettor buy leaderboard position by
// sizing up" — so a stake on this record would only ever be a temptation to
// weight by it.
//
// Odds, probabilities and lines are floats, per the same sentence of §12 read the
// other way.
//
// # The shape follows payload.go, deliberately
//
// schema_version first, an independently versioned document inside an
// independently versioned envelope, timestamps carried from the source rather
// than stamped fresh. Three topics with three record shapes that follow one
// convention cost a reader one act of learning.
package settlement

import (
	"fmt"
	"math"
	"time"

	"github.com/anpl1623/sharpline/internal/analytics/clv"
	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/domain/odds"
)

// CLVSchemaVersion is the version of the [LegCLV] document.
//
// Versioned independently of kafka.EnvelopeVersion, which versions the frame
// around it. Adding an optional field is not a bump; removing, renaming, or
// changing the meaning or the UNIT of one is — and on this record "changing the
// meaning" explicitly includes changing the closing-price definition, because a
// consumer comparing two builds' output would otherwise attribute a definitional
// change to an arithmetic one.
const CLVSchemaVersion = 1

// CLVMessageType is the kafka.Message.Type stamped on every record this package
// writes to signals.clv. Consumers switch on it.
const CLVMessageType = "clv.measured.v1"

// LegCLV is one graded leg's closing line value, complete enough to reproduce.
//
// Every field is the wire form of a column of wager_leg_clv, in the same order,
// plus the two lookback parameters that table does not store. The parallel is
// deliberate: a phase-12 job that reads this topic and writes that table should
// be a projection rather than a translation.
type LegCLV struct {
	// SchemaVersion is [CLVSchemaVersion] as written.
	SchemaVersion int `json:"schema_version"`

	// LegID is the measurement's identity. wager_leg_clv's primary key is leg_id
	// alone, so a consumer seeing this value twice is seeing a recomputation of
	// one measurement rather than two measurements.
	LegID domain.LegID `json:"leg_id"`

	// WagerID is the ticket, and it is the RECORD KEY on signals.clv. Keying by
	// wager rather than by market is what co-partitions this topic with
	// wager.events, so a wager's placement, settlement and CLV stay ordered
	// relative to one another for a consumer building a user's record.
	WagerID domain.WagerID `json:"wager_id"`

	// UserID owns the wager. It is on the record because the leaderboard is a
	// per-user aggregate and a consumer that had to join back to `wagers` for the
	// owner would not be able to build one from this topic alone.
	UserID domain.UserID `json:"user_id"`

	// EventID, MarketID, MarketType, SelectionID and LeagueID locate the measured
	// price in the catalogue. LeagueID is carried rather than derived because the
	// per-league CLV breakdown is one of the things this topic exists to make
	// possible without a catalogue join.
	EventID     domain.EventID     `json:"event_id"`
	MarketID    domain.MarketID    `json:"market_id"`
	MarketType  domain.MarketType  `json:"market_type"`
	SelectionID domain.SelectionID `json:"selection_id"`
	LeagueID    domain.LeagueID    `json:"league_id"`

	// TakenBookID and ClosingBookID are where the two prices came from. Under the
	// closing-price rules in internal/analytics/clv/doc.go §3 they are the SAME
	// book, and both are carried anyway — a consumer must be able to see that
	// rather than assume it, and the fields are what a future consensus-close
	// definition would change.
	TakenBookID   domain.BookID `json:"taken_book_id"`
	ClosingBookID domain.BookID `json:"closing_book_id"`

	// DevigMethod is the margin model that ACTUALLY produced both fair
	// probabilities. One value for both sides is the invariant: comparing a
	// Shin-devigged take against a multiplicatively devigged close measures the
	// difference between two devig methods, not closing line value.
	DevigMethod odds.DevigMethod `json:"devig_method"`

	// ClosingLookbackSeconds and TakenLookbackSeconds are the two declared
	// parameters of the closing-price definition: how far before the closing
	// instant a quote may lie and still be the close, and how far back the market
	// at placement may be reconstructed from.
	//
	// They are ON THE RECORD, not in a config file somebody has to be told about,
	// because they are the difference between two implementations agreeing and two
	// implementations being handed different questions.
	ClosingLookbackSeconds float64 `json:"closing_lookback_seconds"`
	TakenLookbackSeconds   float64 `json:"taken_lookback_seconds"`

	// TakenLine and ClosingLine are the market lines of the two snapshots, in the
	// MARKET's frame (domain.Line semantics; absent encodes as null). They are
	// equal unless LineMoved is set, and both are carried so a user interface can
	// show "you took −3, it closed −3.5" — which is the entire purpose of a
	// line-moved record.
	TakenLine   domain.Line `json:"taken_line"`
	ClosingLine domain.Line `json:"closing_line"`

	// TakenAt and ClosedAt are the two observation instants, both from the
	// PROVIDER's clock and both the newest quote in their snapshot. ClosedAt is
	// never before TakenAt.
	TakenAt  time.Time `json:"taken_at"`
	ClosedAt time.Time `json:"closed_at"`

	// TakenDecimal is the price the customer was actually sold, MARGIN INCLUDED.
	// It is not part of the arithmetic and is carried so a reader can see the
	// quoted price beside what it was worth — the gap between it and TakenPrice is
	// the book's hold on that side, which is the single most useful thing this
	// record can show a bettor.
	TakenDecimal float64 `json:"taken_decimal"`

	// TakenFair and ClosingFair are the devigged probabilities that were compared,
	// and TakenPrice and ClosingPrice their decimal forms, 1/p. These are FAIR
	// prices, not the quotes the books displayed.
	TakenFair    float64 `json:"taken_fair"`
	ClosingFair  float64 `json:"closing_fair"`
	TakenPrice   float64 `json:"taken_price"`
	ClosingPrice float64 `json:"closing_price"`

	// ProbabilityCLV is ClosingFair − TakenFair in probability points, and
	// PercentCLV is (TakenPrice/ClosingPrice − 1) × 100 in percentage points. They
	// always agree in sign; odds/clv.go carries the proof.
	ProbabilityCLV float64 `json:"probability_clv"`
	PercentCLV     float64 `json:"percent_clv"`

	// Magnitude is |PercentCLV|, unsigned — the direction is in BeatClose and is
	// deliberately not repeated in the sign.
	Magnitude float64 `json:"magnitude"`

	// BeatClose is ProbabilityCLV > odds.CLVTieBand. A TIE IS NOT A BEAT, and the
	// band exists so that a last-bits disagreement between two devig
	// implementations of the same method is not crowned a win.
	BeatClose bool `json:"beat_close"`

	// LineMoved reports that the two snapshots were taken at different lines, so
	// this measurement came from odds.EvaluateCLVAcrossLineMove and is INDICATIVE
	// ONLY. odds.AggregateCLV excludes it, the leaderboard query filters it, and a
	// consumer that ranks by it has reintroduced the bug both of those exist to
	// prevent.
	LineMoved bool `json:"line_moved"`

	// LegStatus is the leg's terminal grading and Voided is (LegStatus == void).
	// A PUSH IS NOT VOID: it is a settlement outcome, not a data problem, and it
	// counts at full weight. Excluding pushes would make a bettor's CLV depend on
	// the scoreboard, which is the dependency CLV exists to remove.
	LegStatus domain.LegStatus `json:"leg_status"`
	Voided    bool             `json:"voided"`

	// GradedAt is when the leg was graded, from the RESULT's own finalisation
	// instant. It is the envelope's ObservedAt, and therefore the instant the
	// staleness SLO measures this record from.
	GradedAt time.Time `json:"graded_at"`

	// ComputedAt is when the measurement was made. It is this system's clock, it
	// is the only value here that is not a provider instant, and nothing keys or
	// compares it — migrations/00009 requires exactly that of a replay key.
	ComputedAt time.Time `json:"computed_at"`
}

// Validate reports whether a decoded record is one this build can read and one
// that describes a coherent measurement.
//
// The version check is exact rather than a floor, matching payload.go's
// reasoning: a record written by a newer build may have changed the meaning of a
// field this build would read confidently and wrongly.
//
// The four identity checks are the ones that earn their place. They are the same
// four migrations/00009 asserts as CHECK constraints, re-run here on the DECODED
// floats — which is where the type system's guarantee no longer reaches, because
// these came off a wire rather than out of odds.EvaluateCLV. Re-deriving
// PercentCLV is deliberately NOT among them, for the migration's own reason: it
// is three chained operations, two evaluators are free to fuse them differently,
// and a check that rejects a correct record one time in ten thousand is worse
// than no check.
func (c LegCLV) Validate() error {
	if c.SchemaVersion != CLVSchemaVersion {
		return fmt.Errorf("settlement: leg %q: CLV schema version %d, this build reads %d",
			c.LegID, c.SchemaVersion, CLVSchemaVersion)
	}
	if _, err := domain.NewLegID(string(c.LegID)); err != nil {
		return fmt.Errorf("settlement: leg CLV record: %w", err)
	}
	if _, err := domain.NewWagerID(string(c.WagerID)); err != nil {
		return fmt.Errorf("settlement: leg %q CLV record: %w", c.LegID, err)
	}
	if _, err := domain.NewUserID(string(c.UserID)); err != nil {
		return fmt.Errorf("settlement: leg %q CLV record: %w", c.LegID, err)
	}
	if !c.LegStatus.IsTerminal() {
		return fmt.Errorf("settlement: leg %q is published with CLV but its status is %s; "+
			"an ungraded leg has nothing to measure", c.LegID, c.LegStatus)
	}
	if !c.DevigMethod.Valid() {
		return fmt.Errorf("settlement: leg %q CLV record: %w", c.LegID, odds.ErrUnknownDevigMethod)
	}

	// probability_clv = closing_fair − taken_fair. A single IEEE-754 subtraction
	// of two doubles, with no association-order freedom and no intermediate
	// rounding, so every implementation of it produces bit-identical results on
	// the same operands. Exact equality is therefore the right comparison and a
	// tolerance would be the wrong one.
	if got := c.ClosingFair - c.TakenFair; got != c.ProbabilityCLV {
		return fmt.Errorf("settlement: leg %q reports a probability CLV of %v; "+
			"closing fair %v minus taken fair %v is %v",
			c.LegID, c.ProbabilityCLV, c.ClosingFair, c.TakenFair, got)
	}
	// magnitude = |percent_clv|. Absolute value is exact.
	if got := math.Abs(c.PercentCLV); got != c.Magnitude {
		return fmt.Errorf("settlement: leg %q reports a magnitude of %v; |%v| is %v",
			c.LegID, c.Magnitude, c.PercentCLV, got)
	}
	// beat_close = (probability_clv > CLVTieBand). A tie is not a beat.
	if want := c.ProbabilityCLV > odds.CLVTieBand; want != c.BeatClose {
		return fmt.Errorf("settlement: leg %q reports beat_close=%t on a probability CLV of %v "+
			"against a tie band of %v", c.LegID, c.BeatClose, c.ProbabilityCLV, odds.CLVTieBand)
	}
	// voided = (leg_status = 'void'). A push is NOT void.
	if want := c.LegStatus == domain.LegStatusVoid; want != c.Voided {
		return fmt.Errorf("settlement: leg %q reports voided=%t with status %s",
			c.LegID, c.Voided, c.LegStatus)
	}

	if c.ClosedAt.Before(c.TakenAt) {
		return fmt.Errorf("settlement: leg %q closed at %s, before the price taken at %s",
			c.LegID,
			c.ClosedAt.UTC().Format(time.RFC3339Nano),
			c.TakenAt.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// newLegCLV assembles the published record from a finished measurement.
//
// Every field is COPIED. The measurement already went through odds.EvaluateCLV,
// which refused to exist unless the comparison was like-for-like, so recomputing
// anything here would be a second implementation of a rule that has already been
// enforced — and the second implementation is the one that would be wrong.
func newLegCLV(m clv.Measurement, closingLookback, takenLookback time.Duration) LegCLV {
	r := m.Result
	return LegCLV{
		SchemaVersion: CLVSchemaVersion,

		LegID:       m.Leg.LegID,
		WagerID:     m.Leg.WagerID,
		UserID:      m.Leg.UserID,
		EventID:     m.Leg.EventID,
		MarketID:    m.Leg.MarketID,
		MarketType:  m.Leg.MarketType,
		SelectionID: m.Leg.SelectionID,
		LeagueID:    m.LeagueID,

		TakenBookID:   r.TakenBook,
		ClosingBookID: r.ClosingBook,
		DevigMethod:   m.DevigMethod,

		ClosingLookbackSeconds: closingLookback.Seconds(),
		TakenLookbackSeconds:   takenLookback.Seconds(),

		TakenLine:   r.Line,
		ClosingLine: r.ClosingLine,
		TakenAt:     r.TakenAt,
		ClosedAt:    r.ClosedAt,

		TakenDecimal: float64(m.Leg.Decimal),
		TakenFair:    float64(r.TakenFair),
		ClosingFair:  float64(r.ClosingFair),
		TakenPrice:   float64(r.TakenPrice),
		ClosingPrice: float64(r.ClosingPrice),

		ProbabilityCLV: r.ProbabilityCLV,
		PercentCLV:     r.PercentCLV,
		Magnitude:      r.Magnitude,
		BeatClose:      r.Beat,
		LineMoved:      r.LineMoved,

		LegStatus: m.Leg.Status,
		Voided:    m.Voided(),

		GradedAt:   m.Leg.GradedAt,
		ComputedAt: m.ComputedAt,
	}
}
