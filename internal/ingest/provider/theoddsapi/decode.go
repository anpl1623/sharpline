package theoddsapi

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain/odds"
	"github.com/anpl1623/sharpline/internal/ingest/normalizer"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// This file is the ONLY provider-specific mapping in the pipeline, and it stops
// at syntax.
//
// internal/ingest/normalizer's raw.go states the rule and the reason:
//
//	"there is exactly one mapping (mapper.go) and the per-provider code stops at
//	 syntax. RawEvent is where the two providers converge. […] two mappings that
//	 must agree eventually stop agreeing, and the disagreement shows up as a
//	 subtly wrong line rather than as a failure."
//
// So this package converts The Odds API's wire shape into normalizer.RawEvent
// and nothing further. Everything downstream — identifier derivation, market
// typing, role assignment, line handling — is shared code in mapping.go that
// reads only the neutral shape and never a theoddsapi type.
//
// # The odds format is NOT in the payload, and that is a hazard
//
// wire.go says it plainly: "-110 and 1.91 are both valid JSON numbers". The
// format is a property of the REQUEST. A consumer reading odds.raw.the-odds-api
// off Kafka therefore cannot tell how to read `price` from the bytes alone.
//
// Two mitigations, both required:
//
//   - RawOutcome.Price is normalised to DECIMAL here, at the only point where
//     the request's oddsFormat is known. Nothing downstream ever sees an
//     American number.
//   - The raw payload published to the bus carries the format as a media-type
//     parameter on its content type (RawContentType), so a future consumer
//     decoding the archived bytes has the same information this decoder had.
//
// A Decoder constructed with a different format than the one the payload was
// fetched with produces silently wrong prices. Construct it from the same
// Config that drove the fetch.

// RawContentType returns the media type for a raw payload captured in the given
// odds format.
//
// The `odds-format` parameter is not decoration. odds.raw.{provider} is the
// replayable record of what the provider said (provider.RawPayload), and a
// replay is only faithful if the replayer knows how to read `price`. Putting the
// format in the content type keeps that fact attached to the bytes rather than
// to whichever process happened to fetch them.
func RawContentType(format OddsFormat) string {
	if !format.Valid() {
		return "application/json"
	}
	return "application/json; odds-format=" + string(format)
}

// Decoder turns odds.raw.the-odds-api payload bytes into the provider-neutral
// shape the normalizer consumes.
//
// It satisfies normalizer.Decoder. That interface is declared by the normalizer
// (its consumer) and implemented here (its producer's package), which is the
// direction CLAUDE.md §12 requires and the direction that keeps the import graph
// acyclic: theoddsapi imports normalizer, never the reverse.
type Decoder struct {
	provider kafka.Provider
	format   OddsFormat
	metrics  *Metrics
}

// NewDecoder returns a decoder for payloads fetched in the given odds format.
//
// format is REQUIRED and is not inferred: see the file comment. Passing the
// wrong one is not detectable from the bytes, so it is rejected at construction
// rather than guessed.
func NewDecoder(format OddsFormat, m *Metrics) (*Decoder, error) {
	if !format.Valid() {
		return nil, fmt.Errorf("%w: odds format %q is neither %q nor %q",
			ErrInvalidConfig, string(format), OddsFormatDecimal, OddsFormatAmerican)
	}
	p, err := kafka.NewProvider(ProviderSlug)
	if err != nil {
		return nil, fmt.Errorf("%w: provider slug: %w", ErrInvalidConfig, err)
	}
	if err := normalizer.ValidateProviderForIdentity(p); err != nil {
		return nil, fmt.Errorf("%w: provider slug: %w", ErrInvalidConfig, err)
	}
	if m == nil {
		// Unregistered collectors: the observe calls stay live and cost
		// nanoseconds, so no call site needs a nil check.
		m, err = NewMetrics(nil)
		if err != nil {
			return nil, err
		}
	}
	return &Decoder{provider: p, format: format, metrics: m}, nil
}

// Provider implements normalizer.Decoder.
func (d *Decoder) Provider() kafka.Provider { return d.provider }

// Format returns the odds format this decoder reads prices in.
func (d *Decoder) Format() OddsFormat { return d.format }

// Decode implements normalizer.Decoder. payload is one event's bytes, exactly as
// they were published to odds.raw.the-odds-api.
func (d *Decoder) Decode(payload []byte) (normalizer.RawEvent, error) {
	var ev EventOdds
	if err := json.Unmarshal(payload, &ev); err != nil {
		return normalizer.RawEvent{}, fmt.Errorf("%w: the-odds-api event payload: %w", ErrMalformedResponse, err)
	}
	raw, dropped, err := RawEventFrom(ev, d.format)
	if err != nil {
		return normalizer.RawEvent{}, err
	}
	// Silence is what makes a lossy decode dangerous. The interface has no
	// channel for a partial result, so the loss is counted instead.
	d.metrics.observeDropped(DropReasonInvalidOdds, dropped)
	return raw, nil
}

var _ normalizer.Decoder = (*Decoder)(nil)

// RawEventFrom converts one decoded provider event into the neutral shape.
//
// The second return value is how many OUTCOMES were dropped because their price
// could not be represented as legal decimal odds. Dropping the outcome rather
// than failing the event is deliberate: one book quoting a nonsense number must
// not remove an entire contest from the board, and the loss is visible in
// sharpline_provider_mapping_dropped_total{reason="invalid_odds"}.
//
// An error is returned only for a payload that is structurally unusable — no
// event identifier, or no league key — because neither can be worked around and
// both would produce an unkeyable record on a compacted topic.
func RawEventFrom(ev EventOdds, format OddsFormat) (normalizer.RawEvent, int, error) {
	id := strings.TrimSpace(ev.ID)
	if id == "" {
		return normalizer.RawEvent{}, 0, fmt.Errorf("%w: event payload carries no id", ErrMalformedResponse)
	}
	// The Odds API's `sport_key` IS the league key: "americanfootball_nfl" is a
	// league, not a sport. The sport is its prefix, which is the provider's own
	// grouping rather than a guess — normalizer.SportKeyFromLeagueKey documents
	// exactly this.
	leagueKey := strings.TrimSpace(ev.SportKey)
	if leagueKey == "" {
		return normalizer.RawEvent{}, 0, fmt.Errorf("%w: event %s carries no sport_key", ErrMalformedResponse, id)
	}

	home := strings.TrimSpace(ev.Home())
	away := strings.TrimSpace(ev.Away())

	out := normalizer.RawEvent{
		ID:           id,
		SportKey:     normalizer.SportKeyFromLeagueKey(leagueKey),
		LeagueKey:    leagueKey,
		LeagueName:   strings.TrimSpace(ev.SportTitle),
		HomeTeam:     home,
		AwayTeam:     away,
		CommenceTime: ev.CommenceTime.Time,
	}
	if home == "" && away == "" {
		// An outright has no competitors to derive a name from, so one must be
		// carried. The provider's own league title ("NFL Super Bowl Winner") is
		// the only text it publishes for such an event; the league key is the
		// fallback. Neither is invented.
		out.Name = firstNonEmpty(strings.TrimSpace(ev.SportTitle), leagueKey)
	}

	dropped := 0
	for _, bk := range ev.Bookmakers {
		key := strings.TrimSpace(bk.Key)
		if key == "" {
			continue
		}
		book := normalizer.RawBook{
			Key:        key,
			Name:       strings.TrimSpace(bk.Title),
			LastUpdate: wireInstant(bk.LastUpdate),
		}
		for _, mk := range bk.Markets {
			mkey := strings.TrimSpace(mk.Key)
			if mkey == "" {
				continue
			}
			market := normalizer.RawMarket{
				Key:        mkey,
				LastUpdate: wireInstant(mk.LastUpdate),
			}
			for _, o := range mk.Outcomes {
				dec, err := decimalOdds(o, format)
				if err != nil {
					dropped++
					continue
				}
				ro := normalizer.RawOutcome{
					Name:        strings.TrimSpace(o.Name),
					Description: strings.TrimSpace(o.Subject()),
					Price:       dec,
				}
				if v, ok := o.Line(); ok {
					// Copied, not aliased: o.Point points into the decoded
					// payload and RawEvent outlives it.
					point := v
					ro.Point = &point
				}
				market.Outcomes = append(market.Outcomes, ro)
			}
			book.Markets = append(book.Markets, market)
		}
		out.Books = append(out.Books, book)
	}
	return out, dropped, nil
}

// decimalOdds converts one outcome's price into decimal odds.
//
// The conversion itself is internal/domain/odds' — CLAUDE.md §10: "Wrong odds
// math is the one bug class that destroys the project's credibility", and a
// second implementation of American → decimal in an adapter is exactly how two
// implementations come to disagree. This function only decides WHICH conversion
// applies, from the format the request declared.
func decimalOdds(o Outcome, format OddsFormat) (float64, error) {
	raw, err := o.rawPrice(format)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(raw) || math.IsInf(raw, 0) {
		return 0, fmt.Errorf("%w: price %v is not finite", ErrMalformedResponse, raw)
	}

	switch format {
	case OddsFormatDecimal:
		d, err := odds.NewDecimal(raw)
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
		}
		return float64(d), nil

	case OddsFormatAmerican:
		// American prices are integers by definition. A fractional one means the
		// response is not in the format the request asked for, which makes every
		// price in it suspect — so it is refused rather than rounded.
		if raw != math.Trunc(raw) {
			return 0, fmt.Errorf("%w: american price %v is not an integer", ErrMalformedResponse, raw)
		}
		a, err := odds.NewAmerican(int64(raw))
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
		}
		d, err := a.Decimal()
		if err != nil {
			return 0, fmt.Errorf("%w: %w", ErrMalformedResponse, err)
		}
		return float64(d), nil

	default:
		return 0, fmt.Errorf("%w: unknown oddsFormat %q", ErrMalformedResponse, string(format))
	}
}

// wireInstant flattens an optional provider timestamp. A nil or zero pointer
// becomes the zero time, which normalizer.RawBook and RawMarket document as
// "absent" — never time.Now(), which would stamp our clock onto an observation
// we did not make.
func wireInstant(t *wireTime) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.Time
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
