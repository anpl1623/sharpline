package theoddsapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// The wire types below mirror The Odds API v4 response schema field for field.
// The authoritative schema is testdata/docsamples/openapi_v4.json, fetched from
// the provider's own SwaggerHub document; wire_test.go reads that file and
// asserts every json tag here names a property the provider actually documents,
// so a typo fails the build instead of silently decoding to a zero value.
//
// Decoding is deliberately NOT strict about unknown fields. The provider's own
// schema says "NOTE Allow for the addition of new market types in future", and
// several optional fields (link, sid, bet_limit, multiplier) appear only when
// the corresponding include* query parameter is set. Rejecting an unknown field
// would turn every additive provider change into an outage.

// OddsFormat is the `oddsFormat` query parameter, and it decides how the
// numbers in `price` must be READ. It is not discoverable from the payload —
// -110 and 1.91 are both valid JSON numbers — so it travels with the request
// and is passed explicitly into conversion.
type OddsFormat string

// The two formats the provider supports (OpenAPI: oddsFormat enum).
const (
	// OddsFormatDecimal is what this package requests. CLAUDE.md §4 and the
	// phase-1 handoff make Decimal the canonical price type in the domain, so
	// asking for decimal means no conversion happens at the edge at all.
	OddsFormatDecimal OddsFormat = "decimal"

	// OddsFormatAmerican is supported because the provider's published example
	// responses — the golden files in testdata/docsamples — are in it, and
	// because an operator may want the raw payload archived in the format a US
	// book quotes. Reading one requires the American -> decimal conversion.
	OddsFormatAmerican OddsFormat = "american"
)

// Valid reports whether f is one of the documented values.
func (f OddsFormat) Valid() bool {
	return f == OddsFormatDecimal || f == OddsFormatAmerican
}

// DateFormat is the `dateFormat` query parameter. This package always requests
// iso, but the decoder accepts both, because the field's JSON type changes with
// this parameter and a decoder that only handles one would fail obscurely if
// the parameter were ever changed.
type DateFormat string

// The two formats the provider supports (OpenAPI: dateFormat enum).
const (
	DateFormatISO  DateFormat = "iso"
	DateFormatUnix DateFormat = "unix"
)

// Valid reports whether f is one of the documented values.
func (f DateFormat) Valid() bool {
	return f == DateFormatISO || f == DateFormatUnix
}

// wireTime decodes a timestamp that the provider documents as EITHER an
// RFC3339 string (dateFormat=iso, the default and what this package requests)
// OR an integer count of seconds since the epoch (dateFormat=unix).
//
// JSON null decodes to the zero time rather than to an error: `/scores` returns
// `"last_update": null` for an event that has not started, and on the odds
// endpoints a missing bookmaker-level timestamp is a normal, documented state
// that the market-level timestamp covers.
type wireTime struct {
	time.Time
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *wireTime) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		t.Time = time.Time{}
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("timestamp is not a JSON string: %w", err)
		}
		if s == "" {
			t.Time = time.Time{}
			return nil
		}
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return fmt.Errorf("timestamp %q is not RFC3339: %w", s, err)
		}
		t.Time = parsed.UTC()
		return nil
	}
	secs, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return fmt.Errorf("timestamp %q is neither a string nor an integer: %w", string(b), err)
	}
	t.Time = time.Unix(secs, 0).UTC()
	return nil
}

// -----------------------------------------------------------------------------
// GET /v4/sports
// -----------------------------------------------------------------------------

// Sport is one entry from the sport catalogue.
//
// This endpoint is FREE (guides/v4 §Usage Quota Costs: "This endpoint does not
// count against the usage quota"), which is why ADR 0003 says to refresh the
// catalogue aggressively — only price polling costs anything.
type Sport struct {
	// Key is the slug used as the {sport} path parameter everywhere else.
	Key string `json:"key"`

	// Group is the broad category ("American Football"), which maps onto the
	// domain's Sport.
	Group string `json:"group"`

	// Title is the presentable league name ("NFL"), which maps onto the
	// domain's League. The provider warns this "can change, for example if a
	// league undergoes a name change" — so Key, not Title, is the stable
	// identifier.
	Title string `json:"title"`

	// Description is a longer human label ("US Football").
	Description string `json:"description"`

	// Active reports whether the sport is in season. The endpoint omits
	// inactive sports unless `all=true` is passed.
	Active bool `json:"active"`

	// HasOutrights reports whether the sport carries futures markets, in which
	// case `markets=outrights` is the valid market rather than h2h/spreads.
	HasOutrights bool `json:"has_outrights"`
}

// -----------------------------------------------------------------------------
// GET /v4/sports/{sport}/odds  and  GET /v4/sports/{sport}/events/{id}/odds
// -----------------------------------------------------------------------------

// EventOdds is one event and every bookmaker's markets for it.
//
// The two endpoints differ only in shape at the top level: the sweep returns an
// array of these, the single-event endpoint returns one. The element type is
// identical, so both decode into this.
type EventOdds struct {
	// ID is the provider's 32-character event identifier. It is stable across
	// the odds and scores endpoints ("the game id field in the scores response
	// matches the game id field in the odds response"), which is what lets
	// settlement join a result back to the market it graded.
	ID string `json:"id"`

	// SportKey is the sport slug, matching Sport.Key.
	SportKey string `json:"sport_key"`

	// SportTitle is the presentable league name. ABSENT from older payloads —
	// the provider's own published /odds example does not carry it — so it must
	// never be treated as required.
	SportTitle string `json:"sport_title"`

	// CommenceTime is the scheduled start. The provider documents the in-play
	// test against it directly: "If commence_time is less than the current
	// time, the event is in-play." That is the input to §5's adaptive polling
	// decision, since a live event polls fast and a futures market polls slow.
	CommenceTime wireTime `json:"commence_time"`

	// HomeTeam and AwayTeam are nullable: the provider documents them as null
	// for outrights (futures) events, and as "one of the participants" for
	// sports where home/away is not meaningful (MMA, tennis).
	HomeTeam *string `json:"home_team"`
	AwayTeam *string `json:"away_team"`

	// HomeRotation and AwayRotation appear only with
	// includeRotationNumbers=true.
	HomeRotation *int `json:"home_rotation"`
	AwayRotation *int `json:"away_rotation"`

	// Bookmakers is one entry per book in the requested regions/bookmakers.
	Bookmakers []Bookmaker `json:"bookmakers"`
}

// InPlay reports whether the event has started as of now, using the provider's
// own documented test.
func (e EventOdds) InPlay(now time.Time) bool {
	return !e.CommenceTime.IsZero() && e.CommenceTime.Before(now)
}

// Bookmaker is one book's view of an event.
type Bookmaker struct {
	// Key is the book slug ("draftkings"), stable and usable as an identifier.
	Key string `json:"key"`

	// Title is the presentable book name ("DraftKings").
	Title string `json:"title"`

	// LastUpdate is when this bookmaker's odds were last read. It is the
	// FALLBACK recency signal, not the preferred one — see Market.LastUpdate.
	// It is absent from the provider's published event-odds example, so it is a
	// pointer and its absence is normal.
	LastUpdate *wireTime `json:"last_update"`

	// Markets is one entry per requested market key that this book prices.
	Markets []Market `json:"markets"`

	// Link and SID appear only with includeLinks/includeSids.
	Link *string `json:"link"`
	SID  *string `json:"sid"`
}

// Market is one betting market at one book.
type Market struct {
	// Key is the market slug: h2h, spreads, totals, outrights on the featured
	// endpoint; player props and period markets on the event-odds endpoint.
	// The provider's schema note asks callers to "allow for the addition of new
	// market types in future", so this is a string and not an enum.
	Key string `json:"key"`

	// LastUpdate is when this market's odds were last read.
	//
	// THIS IS THE PROVIDER'S observed_at, and it is the subtrahend in the
	// headline staleness SLO. The provider's own schema says so explicitly: "To
	// check recency of odds, we recommend using this field instead of the
	// 'last_update' field at the bookmaker level."
	//
	// It is a pointer because the field is newer than the bookmaker-level one:
	// the provider's published /odds example carries only the bookmaker-level
	// timestamp. ObservedAt implements the fallback.
	LastUpdate *wireTime `json:"last_update"`

	// Outcomes are the selections. Two for h2h/spreads/totals on a US sport,
	// three where a draw is possible, many for outrights and props.
	Outcomes []Outcome `json:"outcomes"`

	// Link and SID appear only with includeLinks/includeSids.
	Link *string `json:"link"`
	SID  *string `json:"sid"`
}

// Outcome is one selection's price.
type Outcome struct {
	// Name is the outcome label: a team name, "Over"/"Under", or "Draw".
	Name string `json:"name"`

	// Price is the odds IN THE FORMAT THE REQUEST ASKED FOR. Read it with
	// DecimalPrice, never directly — a raw -110 read as decimal odds is a
	// catastrophic and entirely silent error.
	Price float64 `json:"price"`

	// Point is the handicap or threshold. Absent for h2h and outrights, which
	// is why it is a pointer: 0 is a legitimate spread and must not be
	// confused with "no line".
	Point *float64 `json:"point"`

	// Description carries extra identity for markets where Name is not
	// sufficient — most importantly the player's name on a player prop, where
	// every outcome is called "Over" or "Under".
	Description *string `json:"description"`

	// Link, SID, BetLimit and Multiplier appear only with the corresponding
	// include* query parameters.
	Link       *string  `json:"link"`
	SID        *string  `json:"sid"`
	BetLimit   *float64 `json:"bet_limit"`
	Multiplier *float64 `json:"multiplier"`
}

// ObservedAt returns the instant the provider says this market's odds were
// read, and reports whether any timestamp was available at all.
//
// Resolution order follows the provider's own recommendation: the market-level
// timestamp wins, the bookmaker-level one is the fallback. The distinction is
// not cosmetic — the bookmaker-level value is when that BOOK was last polled,
// which can be materially older than when this particular market changed.
//
// The second return value is false when neither is present. That case must not
// be papered over with time.Now(): stamping our own clock onto a price we did
// not observe would make the staleness SLO measure zero provider latency and
// report perfect freshness for data of unknown age. The caller decides —
// dropping the market is a defensible answer, silently inventing an
// observation instant is not.
func (m Market) ObservedAt(book Bookmaker) (time.Time, bool) {
	if m.LastUpdate != nil && !m.LastUpdate.IsZero() {
		return m.LastUpdate.Time, true
	}
	if book.LastUpdate != nil && !book.LastUpdate.IsZero() {
		return book.LastUpdate.Time, true
	}
	return time.Time{}, false
}

// DecimalPrice converts this outcome's price into decimal odds, which is the
// canonical representation everywhere downstream.
//
// It duplicates no math from internal/domain/odds: that package is the single
// implementation of American <-> decimal, and CLAUDE.md §10 is explicit about
// what a second, disagreeing implementation of the odds math would cost. This
// function only decides WHICH conversion applies, from the request's declared
// format, and delegates. See adapter.go for the call into the domain.
//
// Returning an error rather than a best guess is deliberate: an unrecognised
// format means the request and the decoder disagree, and every price in the
// payload is then suspect.
func (o Outcome) rawPrice(format OddsFormat) (float64, error) {
	if !format.Valid() {
		return 0, fmt.Errorf("%w: unknown oddsFormat %q", ErrMalformedResponse, string(format))
	}
	return o.Price, nil
}

// Line returns the outcome's handicap or threshold and whether it has one.
func (o Outcome) Line() (float64, bool) {
	if o.Point == nil {
		return 0, false
	}
	return *o.Point, true
}

// Subject returns the description field, which names the player on a player
// prop and is empty on every featured market.
func (o Outcome) Subject() string {
	if o.Description == nil {
		return ""
	}
	return *o.Description
}

// Home returns the home team name, or "" when the provider sent null (futures).
func (e EventOdds) Home() string {
	if e.HomeTeam == nil {
		return ""
	}
	return *e.HomeTeam
}

// Away returns the away team name, or "" when the provider sent null (futures).
func (e EventOdds) Away() string {
	if e.AwayTeam == nil {
		return ""
	}
	return *e.AwayTeam
}
