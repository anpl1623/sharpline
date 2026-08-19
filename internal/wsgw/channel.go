// The channel grammar: the three subscription names CLAUDE.md §5 defines, and
// the one function that decides which of them a market belongs to.
//
// # Why the split back into (kind, id) is safe
//
// A channel is `kind:id`, and the split is a single strings.Cut on the FIRST
// colon. That is unambiguous because internal/domain/ids.go excludes ':' from
// the identifier charset, and it says so in exactly these terms:
//
//	"The charset excludes ':' for a load-bearing reason: CLAUDE.md §5 defines
//	 WebSocket channels as `event:{id}` and `market:{id}`. If an identifier
//	 could contain a colon, splitting a channel name back into (kind, id) would
//	 be ambiguous, and the ambiguity would surface as cross-subscription leakage
//	 rather than as a parse error."
//
// This file is the other half of that decision. It parses through
// domain.NewEventID, domain.NewMarketID and domain.NewSlug rather than through
// a local charset check, so the guarantee is enforced in one place and a future
// widening of the identifier charset breaks the constructors' tests rather than
// quietly making two subscriptions collide.
//
// # One function decides a market's channels
//
// [ChannelsFor] is the only place that says which channels a market is
// published on, and both the publish path and the snapshot path call it. If
// they each derived the set, a market could be delivered on `event:X` but
// snapshotted only on `market:Y`, and the defect would present as a board that
// is correct on connect and drifts afterwards — which is the hardest possible
// version of this bug to see.
package wsgw

import (
	"fmt"
	"strings"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// MaxChannelLen bounds a channel string before it is parsed.
//
// It is the longest legal channel plus headroom: the longest kind is "market"
// (6), plus the colon, plus domain.MaxIDLen (128). Checked FIRST, so a
// megabyte string is refused without being examined byte by byte.
const MaxChannelLen = len("market") + 1 + domain.MaxIDLen

// ChannelKind is the prefix of a channel name. A closed set: CLAUDE.md §5 names
// exactly three, and a fourth would be a protocol change rather than a feature
// flag.
type ChannelKind string

// The channel kinds.
const (
	// ChannelEvent subscribes to every market on one event — the event detail
	// page's subscription.
	ChannelEvent ChannelKind = "event"

	// ChannelMarket subscribes to one market. The finest grain, and the one a
	// bet slip watches so a price change on a leg can be detected without
	// carrying the rest of the event.
	ChannelMarket ChannelKind = "market"

	// ChannelLeague subscribes to every market in one league — the odds board's
	// subscription, and the widest thing a single client can ask for. It is
	// keyed by SLUG rather than by league id because the slug is what appears
	// in a URL (domain.Slug says so), so the board's route and its subscription
	// are the same string.
	ChannelLeague ChannelKind = "league"
)

// String implements fmt.Stringer.
func (k ChannelKind) String() string { return string(k) }

// Valid reports whether k is one of the three kinds.
func (k ChannelKind) Valid() bool {
	switch k {
	case ChannelEvent, ChannelMarket, ChannelLeague:
		return true
	default:
		return false
	}
}

// ChannelKinds returns the kinds in a stable order.
func ChannelKinds() []ChannelKind { return []ChannelKind{ChannelEvent, ChannelMarket, ChannelLeague} }

// Channel is a validated subscription name.
//
// The fields are unexported so a Channel can only come from [ParseChannel] or
// from one of the typed constructors, all of which run the domain validators.
// That makes "every Channel in the routing table names a syntactically valid
// entity" a property of the type rather than of the code paths that happen to
// build one — the same argument internal/domain makes for its identifier types.
//
// It is comparable, so it is usable directly as a map key. The routing table and
// the snapshot map both want exactly that.
type Channel struct {
	kind ChannelKind
	id   string
}

// Kind returns the channel's kind.
func (c Channel) Kind() ChannelKind { return c.kind }

// ID returns the channel's identifier, without the kind prefix.
func (c Channel) ID() string { return c.id }

// IsZero reports whether the channel is unset. The zero Channel is never
// produced by a constructor, so a zero value always means "nothing was parsed".
func (c Channel) IsZero() bool { return c.kind == "" || c.id == "" }

// String returns the wire form, `kind:id`. The zero Channel renders as the empty
// string rather than as ":" so it cannot be mistaken for a real subscription in
// a log line.
func (c Channel) String() string {
	if c.IsZero() {
		return ""
	}
	return string(c.kind) + ":" + c.id
}

// MarshalText implements encoding.TextMarshaler, which is what makes a Channel
// render as a JSON string rather than as an object.
//
// A zero Channel is an ERROR rather than an empty string. It can only arise from
// a hub bug — a frame assembled without a channel — and failing the marshal
// fails the whole frame loudly, where an empty string would ship a frame naming
// a channel nobody can subscribe to.
func (c Channel) MarshalText() ([]byte, error) {
	if c.IsZero() {
		return nil, fmt.Errorf("%w: zero-valued channel", ErrInvalidFrame)
	}
	return []byte(c.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *Channel) UnmarshalText(b []byte) error {
	parsed, err := ParseChannel(string(b))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}

// EventChannel builds the channel carrying every market on one event.
func EventChannel(id domain.EventID) (Channel, error) {
	if _, err := domain.NewEventID(id.String()); err != nil {
		return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}
	return Channel{kind: ChannelEvent, id: id.String()}, nil
}

// MarketChannel builds the channel carrying one market.
func MarketChannel(id domain.MarketID) (Channel, error) {
	if _, err := domain.NewMarketID(id.String()); err != nil {
		return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}
	return Channel{kind: ChannelMarket, id: id.String()}, nil
}

// LeagueChannel builds the channel carrying every market in one league.
func LeagueChannel(slug domain.Slug) (Channel, error) {
	if _, err := domain.NewSlug(slug.String()); err != nil {
		return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}
	return Channel{kind: ChannelLeague, id: slug.String()}, nil
}

// ParseChannel turns a client-supplied string into a Channel.
//
// The returned error wraps [ErrInvalidChannel] and is BOUNDED: it never contains
// the input. A client is told which classification failed through
// [ChannelRejectReason], which is a value from a closed set, and the input is
// echoed — bounded, through [SafeEcho] — only on the ack's `rejected` list where
// the client needs it to correlate. That split is the point: the metric label and
// the log line take the classification, and only the frame the client already
// asked for takes the value.
func ParseChannel(s string) (Channel, error) {
	if s == "" {
		return Channel{}, fmt.Errorf("%w: empty", ErrInvalidChannel)
	}
	// Length first, so nothing downstream ever scans an unbounded string.
	if len(s) > MaxChannelLen {
		return Channel{}, fmt.Errorf("%w: %d bytes, limit is %d", ErrInvalidChannel, len(s), MaxChannelLen)
	}

	rawKind, id, found := strings.Cut(s, ":")
	if !found {
		return Channel{}, fmt.Errorf("%w: expected kind:id", ErrInvalidChannel)
	}

	switch kind := ChannelKind(rawKind); kind {
	case ChannelEvent:
		eventID, err := domain.NewEventID(id)
		if err != nil {
			return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
		}
		return Channel{kind: ChannelEvent, id: eventID.String()}, nil

	case ChannelMarket:
		marketID, err := domain.NewMarketID(id)
		if err != nil {
			return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
		}
		return Channel{kind: ChannelMarket, id: marketID.String()}, nil

	case ChannelLeague:
		slug, err := domain.NewSlug(id)
		if err != nil {
			return Channel{}, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
		}
		return Channel{kind: ChannelLeague, id: slug.String()}, nil

	default:
		// The kind is NOT echoed. The supported set is enough to fix the
		// mistake, and the value is a client-controlled string that would
		// otherwise reach a log line.
		return Channel{}, fmt.Errorf("%w: kind must be one of event, market, league", ErrInvalidChannel)
	}
}

// ChannelRejectReason classifies a ParseChannel failure for the ack frame and
// for sharpline_ws_channel_rejects_total.
//
// It works from the input rather than from the error's text, because the error's
// text is built from untrusted input and a switch over it would be a switch over
// something a stranger wrote. The classification is coarse on purpose: a client
// needs to know whether it sent the wrong shape, the wrong kind or a bad
// identifier, and a finer split would put more of the input's structure into a
// Prometheus label.
func ChannelRejectReason(raw string) RejectReason {
	if len(raw) > MaxChannelLen {
		return RejectTooLong
	}
	rawKind, _, found := strings.Cut(raw, ":")
	if !found {
		return RejectMalformed
	}
	if !ChannelKind(rawKind).Valid() {
		return RejectUnknownKind
	}
	return RejectInvalidID
}

// ChannelsFor returns every channel one computed market is published on, in a
// stable order: market, then event, then league.
//
// It is THE definition, called by the publish path and by the snapshot path, so
// the two cannot disagree — see the file comment for why a disagreement here is
// the hardest version of this bug to notice.
//
// It returns an error rather than skipping a channel it cannot build. A market
// whose event id or league slug does not validate is a market that cannot be
// routed to two of its three audiences, and holding it anyway would put a market
// in the slate that some subscribers can never be told about. state.go therefore
// REJECTS such a record rather than storing a partially routable one — which is
// the same judgement pricing.NewMarketSnapshot makes about a record whose parts
// do not cross-check.
//
// The order is deliberate: market first because it is the narrowest and the one
// a bet slip watches, league last because it is the widest.
func ChannelsFor(c pricing.ComputedMarket) ([]Channel, error) {
	marketID, err := c.MarketID()
	if err != nil {
		return nil, fmt.Errorf("%w: market id: %w", ErrInvalidChannel, err)
	}
	market, err := MarketChannel(marketID)
	if err != nil {
		return nil, err
	}

	eventID, err := domain.NewEventID(c.Event.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: event id: %w", ErrInvalidChannel, err)
	}
	event, err := EventChannel(eventID)
	if err != nil {
		return nil, err
	}

	slug, err := domain.NewSlug(c.League.Slug)
	if err != nil {
		return nil, fmt.Errorf("%w: league slug: %w", ErrInvalidChannel, err)
	}
	league, err := LeagueChannel(slug)
	if err != nil {
		return nil, err
	}

	return []Channel{market, event, league}, nil
}
