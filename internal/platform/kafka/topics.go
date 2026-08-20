// The topic registry: the Go-side mirror of what Terraform declares.
//
// CLAUDE.md §3 fixes the four original topics and their retention posture:
//
//	odds.raw.{provider}   retention-based
//	odds.normalized       compacted, keyed by market
//	price.computed        compacted
//	wager.events          retention-based, the settlement audit trail
//
// Phase 9 adds the SIGNALS FAMILY. Three of the four names come straight out of
// §3's event-flow diagram, which draws the phase-12 Flink jobs emitting
// `signals.steam | signals.arb | signals.clv`:
//
//	signals.ev            retention-based, keyed by market
//	signals.arb           retention-based, keyed by market
//	signals.steam         retention-based, keyed by market
//	signals.clv           retention-based, keyed by wager
//
// SIGNALS.EV IS AN ADDITION TO THE CHARTER AND IS FLAGGED AS ONE. The diagram does
// not name it, but §6's Analytics bullet leads with "Positive-EV finder against a
// sharp reference book", and phase 9 needs it as a first-class signal for the same
// reason the other three are: a +EV finding is an event that has to reach a
// subscriber, be recorded, and be replayable. Leaving it off the bus would make
// the +EV finder the one analytic that phase 12 could not replace like for like.
//
// ALL FOUR ARE RETENTION-BASED, NOT COMPACTED, and that is the load-bearing
// decision rather than an oversight. Compaction keeps the latest record per key,
// which is only meaningful when the latest record SUPERSEDES the earlier ones --
// true of a market's current line, false of a finding. "The latest steam move for
// market X" is not a snapshot of anything; it is one event, and the one before it
// is a different event that also happened. Compacting these would do to the signal
// history exactly what §3 says compacting wager.events would do to the audit trail.
//
// # Two sources of truth, split along what each one is for
//
// deploy/terraform/modules/kafka-topics owns everything the BROKER needs:
// partition counts, retention.ms, segment.bytes, min.cleanable.dirty.ratio,
// max.compaction.lag.ms. CLAUDE.md §9 is emphatic that this is Terraform's job
// and says why — "Kafka topic configuration is exactly the kind of thing that
// gets created once with a CLI flag, forgotten, and then silently differs
// between laptop and cluster."
//
// This file records only what the CODE has to reason about, which is two things
// and no more:
//
//   - whether a topic is COMPACTED, because that decides whether a null value
//     means anything (a tombstone) and whether a from-the-start read is a
//     snapshot;
//   - which DOMAIN IDENTIFIER it is keyed by, because that is what makes the
//     publish signatures type-safe.
//
// Neither of those can be read from the broker at compile time, and neither is a
// tuning parameter. Retention milliseconds and segment sizes are deliberately
// NOT duplicated here: a second copy of a number that Terraform owns is a second
// copy that drifts.
package kafka

import (
	"fmt"
	"strings"
)

// Topic names. Frozen contract, shared with Terraform's
// var.topics / var.raw_providers, with the compose stack's kafka-ui, and with
// every consumer group's subscription list. Renaming one is a breaking change
// that orphans a compacted log.
const (
	// TopicOddsNormalized is the current-line snapshot: compacted, keyed by
	// market_id. CLAUDE.md §3 — "a compacted topic keyed by market_id IS the
	// current-line snapshot, replayable from scratch, which removes a whole
	// class of cache-coherency bugs between the bus and Redis."
	TopicOddsNormalized = "odds.normalized"

	// TopicPriceComputed carries pricer output — fair value, EV, Kelly — and is
	// compacted for the same reason and keyed the same way, so that `stream` can
	// build a client's initial snapshot from the log alone.
	TopicPriceComputed = "price.computed"

	// TopicWagerEvents is the settlement audit trail: retention-based, keyed by
	// wager_id so a wager's placement, cash-out and settlement stay ordered
	// relative to each other. NOT compacted — the whole value of an audit trail
	// is that superseded entries survive.
	TopicWagerEvents = "wager.events"

	// TopicOddsRawPrefix prefixes the per-provider raw payload topics. The full
	// name is built by OddsRaw; the prefix is exported because kafka-ui users
	// and operators pattern-match on it.
	TopicOddsRawPrefix = "odds.raw."

	// TopicSignalsEV carries positive-EV findings from the pricer's signals
	// stage. Retention-based and keyed by market_id.
	//
	// Keyed by MARKET rather than by selection even though a finding is about one
	// selection, and the reason is co-partitioning: odds.normalized and
	// price.computed are both keyed by market_id with the same partition count, so
	// a market-keyed signals topic lands the same market on the same partition
	// INDEX across all three. Phase 12's Flink jobs join these streams, and
	// co-partitioned sources on the join key is the difference between a local
	// join and a network shuffle. Keying by selection would break that for the
	// sake of a finer granularity nothing consumes.
	TopicSignalsEV = "signals.ev"

	// TopicSignalsArb carries arbitrage findings. Retention-based, keyed by
	// market_id — which is also the natural key here rather than a compromise: an
	// arbitrage is a statement about ONE market's outcome set across books, so
	// the market is what it is about.
	TopicSignalsArb = "signals.arb"

	// TopicSignalsSteam carries steam-move detections. Retention-based, keyed by
	// market_id, which is what CLAUDE.md §3 asks for in as many words: "hopping
	// window over line-movement velocity, keyed by market, across books".
	//
	// Note that the DATABASE key is finer — migration 00009 keys steam_signals by
	// (market, selection, window) because steam is directional. The bus key stays
	// at the market so that all of one market's signals stay ordered relative to
	// each other on one partition; the selection travels in the payload.
	TopicSignalsSteam = "signals.steam"

	// TopicSignalsCLV carries per-graded-leg closing line value, written by
	// `settle`. Retention-based and keyed by WAGER, not by market — the only
	// topic in the signals family that is.
	//
	// The other three are findings about a market and are consumed alongside the
	// market streams. A CLV record is a fact about a WAGER: odds/clv.go says "the
	// settle service writes one per graded leg". Keying it by wager_id makes it
	// co-partitioned with wager.events, so a wager's placement, settlement and CLV
	// stay ordered relative to one another for a consumer building a user's
	// record — which is exactly what the leaderboard does. Keying by market would
	// scatter one wager's legs across partitions with no ordering between them.
	TopicSignalsCLV = "signals.clv"
)

// maxTopicNameLen is Kafka's own limit on a topic name.
const maxTopicNameLen = 249

// Retention is what the broker does with a superseded record.
type Retention uint8

const (
	// RetentionUnknown is the zero value and is never a valid retention. A
	// Topic that reports it did not come from this registry.
	RetentionUnknown Retention = iota

	// RetentionDelete is cleanup.policy=delete: records age out by time or size
	// and the log is a replayable window of history.
	RetentionDelete

	// RetentionCompact is cleanup.policy=compact: the broker eventually keeps
	// only the latest record per key, so the log converges on a snapshot.
	RetentionCompact
)

// String implements fmt.Stringer using Kafka's own cleanup.policy spelling, so
// a log line here and a `--describe` in kafka-ui use the same word.
func (r Retention) String() string {
	switch r {
	case RetentionDelete:
		return "delete"
	case RetentionCompact:
		return "compact"
	default:
		return "unknown"
	}
}

// Valid reports whether r is a real retention posture.
func (r Retention) Valid() bool { return r == RetentionDelete || r == RetentionCompact }

// KeyKind names the domain identifier a topic's record key holds.
//
// It exists so a Delivery can refuse to hand back the wrong kind of identifier.
// The PRODUCE side needs no runtime check at all — the publish methods take the
// concrete domain type, so a mis-keyed publish does not compile.
type KeyKind uint8

const (
	// KeyKindUnknown is the zero value: a topic outside this registry.
	KeyKindUnknown KeyKind = iota
	// KeyKindMarketID means the key is a domain.MarketID.
	KeyKindMarketID
	// KeyKindEventID means the key is a domain.EventID.
	KeyKindEventID
	// KeyKindWagerID means the key is a domain.WagerID.
	KeyKindWagerID
)

// String implements fmt.Stringer.
func (k KeyKind) String() string {
	switch k {
	case KeyKindMarketID:
		return "market_id"
	case KeyKindEventID:
		return "event_id"
	case KeyKindWagerID:
		return "wager_id"
	default:
		return "unknown"
	}
}

// Topic is a declared topic and the two properties this package's behaviour
// depends on. Its fields are unexported and there is no setter, so a Topic
// cannot be forged with a retention posture the broker does not actually have —
// which matters, because Compacted() is what authorises a tombstone.
type Topic struct {
	name      string
	retention Retention
	key       KeyKind
}

// Name is the Kafka topic name.
func (t Topic) Name() string { return t.name }

// Retention reports the broker's cleanup policy for this topic.
func (t Topic) Retention() Retention { return t.retention }

// Compacted reports whether the broker collapses this log to the latest record
// per key. Tombstones and snapshot reads are meaningful only when it is true.
func (t Topic) Compacted() bool { return t.retention == RetentionCompact }

// KeyKind reports which domain identifier this topic's keys hold.
func (t Topic) KeyKind() KeyKind { return t.key }

// IsZero reports whether t is the zero Topic, i.e. did not come from this
// registry.
func (t Topic) IsZero() bool { return t.name == "" }

// String implements fmt.Stringer.
func (t Topic) String() string {
	if t.IsZero() {
		return "kafka.Topic(zero)"
	}
	return fmt.Sprintf("%s[%s,key=%s]", t.name, t.retention, t.key)
}

// OddsNormalized returns the odds.normalized topic.
//
// Constructors rather than package-level variables: CLAUDE.md §12 forbids global
// mutable state, and an exported var of a struct type is mutable however
// carefully its fields are hidden.
func OddsNormalized() Topic {
	return Topic{name: TopicOddsNormalized, retention: RetentionCompact, key: KeyKindMarketID}
}

// PriceComputed returns the price.computed topic.
func PriceComputed() Topic {
	return Topic{name: TopicPriceComputed, retention: RetentionCompact, key: KeyKindMarketID}
}

// WagerEvents returns the wager.events topic.
func WagerEvents() Topic {
	return Topic{name: TopicWagerEvents, retention: RetentionDelete, key: KeyKindWagerID}
}

// SignalsEV returns the signals.ev topic (phase 9, the +EV finder).
//
// RetentionDelete like every member of the signals family: a finding is a
// point-in-time event, not a current-state snapshot, so there is nothing for
// compaction to supersede. See this file's package comment.
func SignalsEV() Topic {
	return Topic{name: TopicSignalsEV, retention: RetentionDelete, key: KeyKindMarketID}
}

// SignalsArb returns the signals.arb topic (phase 9, arbitrage detection).
func SignalsArb() Topic {
	return Topic{name: TopicSignalsArb, retention: RetentionDelete, key: KeyKindMarketID}
}

// SignalsSteam returns the signals.steam topic (phase 9, steam detection).
func SignalsSteam() Topic {
	return Topic{name: TopicSignalsSteam, retention: RetentionDelete, key: KeyKindMarketID}
}

// SignalsCLV returns the signals.clv topic (phase 9, closing line value).
//
// The one signals topic keyed by wager rather than by market, so that a wager's
// CLV stays ordered against its own placement and settlement on wager.events.
func SignalsCLV() Topic {
	return Topic{name: TopicSignalsCLV, retention: RetentionDelete, key: KeyKindWagerID}
}

// Provider is a provider slug as it appears in odds.raw.{provider}.
//
// # Why this is not domain.Slug
//
// domain.Slug permits underscores. Terraform's kafka-topics module does not:
// its raw_providers validation is `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$` and it
// explains that mixing `.` and `_` in one Kafka topic name is a
// metric-collision hazard (Kafka's own JMX metric names substitute one for the
// other). A slug this package accepted and Terraform rejected would produce a
// service that publishes to a topic that does not exist and cannot be created,
// with auto-creation disabled. So the narrower rule is enforced here, on
// purpose, and the two validations are one contract.
//
// The two slugs in use are "synthetic" and "the-odds-api" (ADR 003 / the
// contract ledger: ingest picks its adapter at startup from the presence of
// ODDS_API_KEY, and both topics exist in every environment so that setting the
// key does not require a terraform apply first).
type Provider string

// NewProvider validates a provider slug against Terraform's charset.
func NewProvider(s string) (Provider, error) {
	if s == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidProvider)
	}
	if len(s)+len(TopicOddsRawPrefix) > maxTopicNameLen {
		return "", fmt.Errorf("%w: %q would exceed Kafka's %d-byte topic name limit",
			ErrInvalidProvider, s, maxTopicNameLen)
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
		switch {
		case alnum:
		case c == '-' && i > 0 && i < len(s)-1:
			// Internal hyphens only: a leading or trailing hyphen would make
			// "odds.raw.-x" or "odds.raw.x-", both of which Terraform rejects.
		default:
			return "", fmt.Errorf("%w: %q (want lowercase alphanumeric with internal hyphens, "+
				"matching Terraform's raw_providers validation)", ErrInvalidProvider, s)
		}
	}
	return Provider(s), nil
}

// String returns the slug as a bare string.
func (p Provider) String() string { return string(p) }

// OddsRaw returns the odds.raw.{provider} topic for one provider.
//
// Keyed by EventID, not MarketID, and that is a decision the normalizer inherits:
// The Odds API returns one payload per EVENT carrying every market on it, so the
// event is the natural unit of a raw record, and keying by it puts all raw
// payloads for one contest on one partition. Per-key ordering is per-partition
// ordering in Kafka, so this is what guarantees the normalizer sees an event's
// payloads in the order they were observed. Keying by market would split one
// payload across partitions or require the producer to shred it before
// normalization, which is exactly the work the normalizer exists to do.
func OddsRaw(provider Provider) (Topic, error) {
	p, err := NewProvider(string(provider))
	if err != nil {
		return Topic{}, err
	}
	return Topic{
		name:      TopicOddsRawPrefix + string(p),
		retention: RetentionDelete,
		key:       KeyKindEventID,
	}, nil
}

// Topics returns the named topics — everything except the per-provider
// odds.raw.* family, whose membership is a deployment decision held in
// Terraform's raw_providers.
//
// Ordered as the pipeline runs: the market stream, then the signals derived from
// it, then the wager stream and the CLV derived from that. Callers that enumerate
// topics for a health check or a kafka-ui bookmark read this order, and pipeline
// order is the one a human reading the list wants.
func Topics() []Topic {
	return []Topic{
		OddsNormalized(),
		PriceComputed(),
		SignalsEV(),
		SignalsArb(),
		SignalsSteam(),
		WagerEvents(),
		SignalsCLV(),
	}
}

// LookupTopic resolves a topic name back to its registry entry, including the
// odds.raw.* family.
//
// It reports false for a name this system does not declare. Callers use that to
// stay permissive about topics they do not own — the integration tests create
// throwaway topics.
//
// The signals.* family IS enumerated as of phase 9. Before that it was the
// standing example of a topic this registry deliberately did not know about,
// which is why the permissiveness exists at all; the permissiveness is kept
// because the property it buys — an unrecognised topic is usable but is never
// treated as compacted — is worth having whether or not anything currently
// depends on it.
func LookupTopic(name string) (Topic, bool) {
	switch name {
	case TopicOddsNormalized:
		return OddsNormalized(), true
	case TopicPriceComputed:
		return PriceComputed(), true
	case TopicWagerEvents:
		return WagerEvents(), true
	case TopicSignalsEV:
		return SignalsEV(), true
	case TopicSignalsArb:
		return SignalsArb(), true
	case TopicSignalsSteam:
		return SignalsSteam(), true
	case TopicSignalsCLV:
		return SignalsCLV(), true
	}
	if suffix, ok := strings.CutPrefix(name, TopicOddsRawPrefix); ok {
		t, err := OddsRaw(Provider(suffix))
		if err != nil {
			return Topic{}, false
		}
		return t, true
	}
	return Topic{}, false
}

// externalTopic wraps a topic name this registry does not declare, so that the
// consumer and the snapshot reader can work with test topics and with topics
// owned by a later phase without this file having to enumerate them.
//
// Retention is reported as RetentionUnknown, which is what keeps an
// unrecognised topic out of the compaction-only code paths unless the caller
// says otherwise.
func externalTopic(name string) Topic {
	return Topic{name: name, retention: RetentionUnknown, key: KeyKindUnknown}
}

// validateTopicName applies Kafka's own rules to a name. Used for topics that
// did not come from the registry — a consumer's subscription list, a snapshot
// target — so that a typo is reported here rather than as a metadata error
// several seconds into a poll loop.
func validateTopicName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty name", ErrInvalidTopic)
	case len(name) > maxTopicNameLen:
		return fmt.Errorf("%w: %q is %d bytes, Kafka's limit is %d",
			ErrInvalidTopic, name, len(name), maxTopicNameLen)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is not a legal Kafka topic name", ErrInvalidTopic, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: %q contains %q; Kafka allows [a-zA-Z0-9._-]",
				ErrInvalidTopic, name, string(rune(c)))
		}
	}
	return nil
}
