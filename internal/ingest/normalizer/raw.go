// The provider-neutral raw shape, and the Decoder seam that produces it.
//
// # Why there is a neutral shape at all
//
// CLAUDE.md §5 mandates one ProviderAdapter interface with a real adapter and a
// synthetic one behind it, and the phase brief requires that the two "produce
// IDENTICAL domain values for equivalent input". The cheap way to get that is to
// give each provider its own path into the domain and then test that the two
// paths agree. The cheap way is wrong: two mappings that must agree eventually
// stop agreeing, and the disagreement shows up as a subtly wrong line rather than
// as a failure.
//
// So there is exactly one mapping (mapper.go) and the per-provider code stops at
// syntax. RawEvent is where the two providers converge. Everything below this
// line in the pipeline is shared code, which makes provider agreement a property
// of the architecture rather than of a test — the test in parity_test.go then
// checks the decoders, which is the part that can actually differ.
//
// # Two conventions this shape fixes, deliberately
//
//  1. RawOutcome.Price is ALWAYS DECIMAL ODDS. The Odds API returns American or
//     decimal depending on a query parameter whose documented default disagrees
//     with every published sample response, so "whatever the provider sent" is
//     not a shape anything downstream can safely consume. Conversion happens
//     inside the decoder, which is the only code that knows what it asked for.
//
//  2. RawMarket.Key uses The Odds API's market-key vocabulary — "h2h",
//     "spreads", "totals", "outrights", "player_*". Not because that provider is
//     privileged, but because inventing a second vocabulary would buy a
//     translation table and nothing else, and because the market key participates
//     in the market identifier (identity.go), so it has to be a published, stable
//     string rather than something a generator picks.
package normalizer

import (
	"fmt"
	"time"

	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// Provider market keys this build maps. Anything else is counted as
// ReasonUnsupportedMarket and skipped.
//
// The four featured keys are documented at the-odds-api.com/liveapi/guides/v4/
// ("Valid values are h2h, spreads, totals, outrights"). Player-prop keys are the
// `player_*` family from the /events/{id}/odds endpoint; they are matched by
// prefix because the family is large and grows, and every member has the same
// shape: name carries Over/Under and description carries the player.
const (
	MarketKeyH2H       = "h2h"
	MarketKeySpreads   = "spreads"
	MarketKeyTotals    = "totals"
	MarketKeyOutrights = "outrights"

	// MarketKeyPlayerPrefix matches the player-prop family.
	MarketKeyPlayerPrefix = "player_"
)

// RawEvent is one provider payload for one contest, after syntax and before
// semantics.
//
// It is the unit of a record on odds.raw.{provider}: kafka/topics.go keys that
// topic by EventID and explains why — "The Odds API returns one payload per EVENT
// carrying every market on it, so the event is the natural unit of a raw record,
// and keying by it puts all raw payloads for one contest on one partition.
// Per-key ordering is per-partition ordering in Kafka, so this is what guarantees
// the normalizer sees an event's payloads in the order they were observed."
//
// The JSON tags are a wire contract with the synthetic adapter, which marshals
// this type directly: the synthetic provider is ours and has no legacy format to
// absorb, so its raw shape is the neutral one and NeutralDecoder is a plain
// unmarshal.
type RawEvent struct {
	// ID is the provider's own event identifier, opaque and stable. It is the
	// input to EventIDFor, so its stability across polls is what makes the
	// compacted topic's keys stable.
	ID string `json:"id"`

	// SportKey groups leagues: "americanfootball". Optional — when empty it is
	// derived from LeagueKey's prefix.
	SportKey string `json:"sport_key,omitempty"`

	// SportName is the display name ("American Football"). Optional; falls back
	// to SportKey.
	SportName string `json:"sport_name,omitempty"`

	// LeagueKey is the provider's league key: "americanfootball_nfl". Required.
	LeagueKey string `json:"league_key"`

	// LeagueName is the display name ("NFL"). Optional; falls back to LeagueKey.
	//
	// The Odds API returns it as `sport_title` on some endpoints and omits it on
	// others, and the free /v4/sports endpoint carries it for every league — ADR
	// 0003 notes that endpoint costs zero credits, so ingest can and should fill
	// this in rather than leaving the board showing "americanfootball_nfl".
	LeagueName string `json:"league_name,omitempty"`

	// Name is the event's display name. Optional: The Odds API publishes none,
	// so for a match the mapper derives "Away at Home", which is the form
	// domain.EventParams.Name documents as typical. An outright MUST carry one,
	// because there are no competitors to derive it from.
	Name string `json:"name,omitempty"`

	// HomeTeam and AwayTeam are the two sides. Both present means
	// domain.EventKindMatch; both absent means EventKindOutright. One without
	// the other is rejected.
	//
	// "Home" fixes the order the home-perspective line convention in
	// domain/market.go depends on; at a neutral venue it is still whichever side
	// the provider lists as home.
	HomeTeam string `json:"home_team,omitempty"`
	AwayTeam string `json:"away_team,omitempty"`

	// CommenceTime is the advertised start. Required.
	CommenceTime time.Time `json:"commence_time"`

	// Books are the bookmakers quoting this event, each with its own markets.
	Books []RawBook `json:"books,omitempty"`
}

// RawBook is one bookmaker's quotes on one event.
type RawBook struct {
	// Key is the provider's bookmaker key: "draftkings". Required, and the
	// input to BookIDFor.
	Key string `json:"key"`

	// Name is the display title ("DraftKings"). Falls back to Key.
	Name string `json:"name,omitempty"`

	// LastUpdate is this bookmaker's observation instant, used for any market
	// under it that carries none of its own.
	//
	// The Odds API has published both shapes: its v4 guide's /odds sample
	// carries last_update on the bookmaker only, while the NFL sample carries it
	// on the bookmaker AND on each market. Preferring the market-level value and
	// falling back to this one is how both are read correctly.
	LastUpdate time.Time `json:"last_update,omitzero"`

	// Reference marks this bookmaker as the catalogue's sharp reference book.
	//
	// It is the provider layer's own statement about the book, and it is the
	// authoritative answer where it exists: internal/pricing derives a market's
	// no-vig fair value from the reference book alone, and records whether the
	// choice came from here (a designation) or from its own configured
	// preference list (a default). Without this field the designation exists at
	// both ends — provider.Catalogue.ReferenceBook() reports it and
	// books.is_reference stores it — and is dropped in the middle.
	//
	// That exactly one book carries it is a property of a catalogue, not of any
	// single RawBook, so nothing here enforces it.
	Reference bool `json:"reference,omitempty"`

	Markets []RawMarket `json:"markets,omitempty"`
}

// RawMarket is one market as one bookmaker quotes it.
type RawMarket struct {
	// Key is the provider's market key. See the MarketKey* constants.
	Key string `json:"key"`

	// LastUpdate is this market's observation instant. Preferred over the
	// bookmaker's when present.
	LastUpdate time.Time `json:"last_update,omitzero"`

	Outcomes []RawOutcome `json:"outcomes,omitempty"`
}

// RawOutcome is one quotable answer within one bookmaker's market.
type RawOutcome struct {
	// Name is the provider's label for the outcome: a competitor name on h2h
	// and spreads, "Over"/"Under" on totals and player props, a runner name on
	// outrights.
	Name string `json:"name"`

	// Description names the individual a player prop is about. Empty for every
	// other market type. It becomes domain.Market.Subject, and it participates
	// in the market identifier, because "David Blough over 0.5 passing TDs" and
	// "Jared Goff over 0.5 passing TDs" are two markets and not two selections.
	Description string `json:"description,omitempty"`

	// Price is DECIMAL ODDS. Always. See the file comment.
	Price float64 `json:"price"`

	// Point is the handicap or threshold, from THIS OUTCOME's own perspective —
	// The Odds API states a spread as +6.5 on one side and -6.5 on the other,
	// which is already what domain.PriceParams.Line wants ("from the selection's
	// own perspective — the value EffectiveLine returns, already inverted for an
	// away spread"). No inversion is applied to it anywhere.
	//
	// A pointer because 0 is a real, frequently-traded line (a pick'em) and
	// absence is a different fact from zero — the same distinction
	// domain.Line draws, for the same reason.
	Point *float64 `json:"point,omitempty"`
}

// Decoder turns one provider's payload bytes into the neutral shape.
//
// Declared here rather than beside its implementations because this package is
// the consumer (CLAUDE.md §12: "interfaces are declared by the consumer, not the
// producer"), and kept to one method because syntax is all a provider gets to
// have an opinion about.
//
// There is deliberately no context.Context: decoding is pure CPU over a byte
// slice already in memory. §12 puts a context first on "anything doing I/O", and
// adding one here would suggest this method might block, which would be a lie
// that a caller could reasonably act on.
type Decoder interface {
	// Provider is the slug this decoder handles. It selects the decoder from the
	// odds.raw.{provider} topic name.
	Provider() kafka.Provider

	// Decode parses one raw record's payload. It must not retain payload.
	Decode(payload []byte) (RawEvent, error)
}

// NeutralDecoder reads the neutral shape itself.
//
// It is what the synthetic provider is decoded with. The synthetic generator
// computes markets live from a seeded model (ADR 0003) and has no wire format of
// its own, so giving it a bespoke one would mean writing a second serialisation
// purely to write a second decoder for it.
//
// It is also the shape a future provider's adapter can target when its payload is
// easier to reshape at the adapter than to model here.
type NeutralDecoder struct{ provider kafka.Provider }

// NewNeutralDecoder returns a decoder for the neutral shape under the given
// provider slug.
func NewNeutralDecoder(p kafka.Provider) (*NeutralDecoder, error) {
	v, err := kafka.NewProvider(string(p))
	if err != nil {
		return nil, fmt.Errorf("%w: provider: %w", ErrInvalidOptions, err)
	}
	return &NeutralDecoder{provider: v}, nil
}

// Provider implements Decoder.
func (d *NeutralDecoder) Provider() kafka.Provider { return d.provider }

// Decode implements Decoder.
func (d *NeutralDecoder) Decode(payload []byte) (RawEvent, error) {
	var ev RawEvent
	if err := strictUnmarshal(payload, &ev); err != nil {
		return RawEvent{}, fmt.Errorf("%w: neutral payload: %w", ErrDecode, err)
	}
	return ev, nil
}

var _ Decoder = (*NeutralDecoder)(nil)
