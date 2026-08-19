// The slate: this replica's in-memory fold of the compacted price.computed
// topic, and the mutex discipline that makes snapshot-then-delta correct.
//
// # Why a fold here, when internal/pricing explicitly refuses one
//
// internal/pricing/state.go rejects "a persistent in-process store of every
// market" and gives two reasons: a second copy of the compacted snapshot
// reintroduces the cache-coherency bug class Kafka was chosen to remove, and it
// makes an HPA'd service stateful.
//
// Neither objection applies to this service, and the difference is worth being
// precise about rather than waving at.
//
//   - The pricer's input is ONE RECORD and its output is a pure function of that
//     record, so it needs no memory of any other market. This gateway's job is
//     to answer "what is the current state of every market on league:nfl" the
//     instant a browser connects. Something must hold that. The only question is
//     what, and the alternatives are worse: re-reading the compacted topic per
//     subscription is a network round trip on every page load, and a Redis
//     mirror is exactly the second copy the charter warns about — with the
//     additional defect that its contents would be a DERIVED view Kafka could
//     no longer correct.
//
//   - It does not make the service stateful in the sense that matters, because
//     the state is not private. Every replica reads EVERY partition (D1, no
//     consumer group), so every replica's fold converges on the same slate from
//     the same log. A pod that dies takes nothing with it; a pod that starts
//     rebuilds by replaying a compacted topic, which is bounded by the size of
//     the board rather than by the age of the deployment. That is what makes
//     CLAUDE.md §9's "no session affinity" true rather than aspirational.
//
// So this file holds a fold, and it holds it in memory only. Redis holds the
// SUBSCRIPTION SET and never the market data (D6); nothing here reads Redis and
// nothing here writes it.
//
// # The lock is the linearisation point (D2)
//
// The API shape is unusual on purpose. [Store.Fold] and [Store.Attach] both take
// a callback and run it while still holding the store's write lock, so:
//
//	Fold    applies a record AND publishes its delta, atomically
//	Attach  reads a channel's snapshot AND registers the subscription, atomically
//
// which makes "no delta can be published between reading a snapshot and
// registering the subscriber" a property of the type rather than an instruction
// in a comment the hub might not follow. doc.go argues why the alternative —
// buffer the deltas, then reconcile against the snapshot — is rejected: it works,
// and it adds an unbounded buffer whose size is set by an external party's
// traffic.
//
// The callbacks must be FAST and must not call back into the store. They run
// under the write lock, so anything that blocks in one blocks every other
// connection's snapshot and the entire fanout. Handing a frame to a bounded,
// non-blocking send queue is exactly the right amount of work; a socket write is
// not, which is a large part of why D4's queue exists at all.
//
// # Nothing is fabricated, ever
//
// A channel with no markets snapshots to an EMPTY, VALID result. A record whose
// document does not validate is REJECTED and counted, not stored — a market a
// client cannot be told the truth about must not be in the slate at all. And the
// payload a client receives is the price.computed envelope's data BYTE FOR BYTE,
// copied out of the fetch buffer and never re-marshalled.
package wsgw

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// Market is one market as the slate holds it.
type Market struct {
	// ID is the market's identifier, validated. It is the slate's key and it is
	// cross-checked against the record's Kafka key on the way in.
	ID domain.MarketID

	// Payload is the price.computed envelope's data, EXACTLY as it arrived.
	//
	// It is what a snapshot and a delta carry, byte for byte. It is a COPY: the
	// bytes a Delivery exposes alias the fetch buffer and internal/platform/kafka
	// states the obligation outright — a handler "MUST NOT retain Delivery.Value()
	// past its return". Retaining the alias would give every client a view of
	// whatever the next fetch decoded into the same memory.
	Payload json.RawMessage

	// Computed is the decoded document.
	//
	// It exists for two jobs and no others: deriving the routing channels, and
	// instrumenting the fanout (the per-price observation instants, the league
	// label, the provider label and ingested_at that
	// sharpline_odds_staleness_seconds needs). It is NEVER re-marshalled to
	// produce what the client sees — that is Payload's job, and keeping the two
	// separate is what stops this package from becoming a second declaration of
	// the pricer's schema.
	Computed pricing.ComputedMarket

	// Channels is the routing set from [ChannelsFor], computed once on apply.
	// Stored rather than recomputed so the teardown on replacement or deletion
	// uses the SAME set that was installed — a recomputed set would miss an
	// index entry whenever a market's event or league changed, and the stale
	// entry would keep delivering the market to a channel it had left.
	Channels []Channel

	// ObservedAt is the record's own observation instant, the value the
	// monotonicity guard compares. It is not re-derived from the payload.
	ObservedAt time.Time
}

// ApplyOutcome is what one record did to the slate. A closed set; the values are
// the `result` label on sharpline_ws_bus_records_total.
type ApplyOutcome string

// The apply outcomes.
const (
	// ApplyStored — the market was added or replaced. The publish callback
	// receives it.
	ApplyStored ApplyOutcome = recordStored

	// ApplyRemoved — a tombstone deleted the key. On a compacted topic that is
	// permanent: no further record for that key is coming, so a consumer that
	// ignores it leaves the market on the board for ever.
	ApplyRemoved ApplyOutcome = recordRemoved

	// ApplyStale — the record was observed strictly before the state already
	// held. Skipped. NOT an error: at-least-once delivery makes a redelivery
	// normal operation, and two replicas reading the same partitions
	// independently makes an out-of-order arrival routine rather than
	// exceptional.
	ApplyStale ApplyOutcome = recordStale

	// ApplyRejected — the record could not be held. Counted, skipped, and the
	// consumer continues; see [ErrRecordRejected].
	ApplyRejected ApplyOutcome = recordRejected

	// ApplyUnsupported — an envelope message type this build does not read.
	ApplyUnsupported ApplyOutcome = recordUnsupported
)

// String implements fmt.Stringer.
func (o ApplyOutcome) String() string { return string(o) }

// ApplyResult describes one fold.
type ApplyResult struct {
	// Outcome is what happened.
	Outcome ApplyOutcome

	// MarketID is the key the record addressed, when it could be determined.
	MarketID domain.MarketID

	// Market is the stored market. Populated only for ApplyStored.
	Market Market

	// Channels are the channels affected.
	//
	// For ApplyStored it is the new market's routing set. For ApplyRemoved it is
	// the set the DELETED entry had — which is the whole reason the set is
	// stored rather than recomputed: a tombstone carries no payload, so there is
	// nothing left to derive an event id or a league slug from, and without this
	// the deletion could only be announced on `market:{id}` and would leave the
	// market on every board that had been showing it.
	//
	// Empty for a tombstone against a key the slate never held. There is nothing
	// to remove and nothing to announce; a delta naming a market the client
	// never received would be noise it has to ignore.
	Channels []Channel
}

// Store is this replica's fold of price.computed.
//
// It is safe for concurrent use. The mutex is exported through the callback API
// rather than as a field, for the reason in the file comment.
type Store struct {
	// mu guards every map below AND serialises the publish/subscribe callbacks.
	// It is an RWMutex only so that a read-only snapshot (a client-requested
	// resync) does not exclude another one; every mutating path and every path
	// that registers a subscriber takes the WRITE lock, so those are totally
	// ordered against each other.
	mu sync.RWMutex

	markets map[domain.MarketID]Market

	// Secondary indexes, so a `league:nfl` snapshot is a lookup rather than a
	// scan of the whole slate. A board page subscribing to three leagues on a
	// Sunday would otherwise walk every market in the system three times, per
	// connection, under the write lock — which is the version of this service
	// that falls over at a few hundred clients rather than at ten thousand.
	byEvent  map[domain.EventID]map[domain.MarketID]struct{}
	byLeague map[domain.Slug]map[domain.MarketID]struct{}

	m *Metrics
}

// NewStore builds an empty slate.
//
// It returns no error, deliberately: there is no configuration here to get
// wrong. m may be nil, in which case every observation is a no-op — the same
// contract [Metrics] states for its own methods.
func NewStore(m *Metrics) *Store {
	return &Store{
		markets:  make(map[domain.MarketID]Market),
		byEvent:  make(map[domain.EventID]map[domain.MarketID]struct{}),
		byLeague: make(map[domain.Slug]map[domain.MarketID]struct{}),
		m:        m,
	}
}

// Fold applies one delivery and, in the SAME critical section, hands the result
// to publish.
//
// This is D2's publish half. Nothing can read the slate or register a subscriber
// between the state changing and the delta being enqueued, because both require
// the lock Fold is holding.
//
// publish is called ONLY when the slate actually changed — ApplyStored, or
// ApplyRemoved for a key that existed. A record that changed nothing is not
// news, and announcing it would put a delta on the wire that every client must
// ignore. It may be nil, which makes Fold a plain apply.
//
// The returned error is non-nil for [ApplyRejected] and [ApplyUnsupported] only.
// The caller MUST NOT propagate it to the consumer loop: internal/platform/kafka
// stops the consumer on a handler error with the offset uncommitted, so one
// poison record on a COMPACTED topic would wedge the fanout for every client for
// ever and be redelivered for ever. Count it, log it, continue.
//
// publish runs under the write lock. It must not block and must not call back
// into the Store; see the file comment.
func (s *Store) Fold(d *kafka.Delivery, publish func(ApplyResult)) (ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.applyLocked(d)
	s.m.observeBusRecord(string(res.Outcome))
	s.m.observeMarketsTracked(len(s.markets))
	if err != nil {
		return res, err
	}

	changed := res.Outcome == ApplyStored ||
		(res.Outcome == ApplyRemoved && len(res.Channels) > 0)
	if publish != nil && changed {
		publish(res)
	}
	return res, nil
}

// Apply folds one delivery with no publication. It is Fold with a nil callback,
// named because a caller that wants only the state change should not have to
// write `nil` and explain it.
func (s *Store) Apply(d *kafka.Delivery) (ApplyResult, error) { return s.Fold(d, nil) }

// applyLocked is Fold's body. The caller holds the write lock.
func (s *Store) applyLocked(d *kafka.Delivery) (ApplyResult, error) {
	if d == nil {
		return ApplyResult{Outcome: ApplyRejected},
			fmt.Errorf("%w: nil delivery", ErrRecordRejected)
	}

	// The key is authoritative for WHICH market a record addresses, so it is
	// read before anything else and a record without a usable one is refused.
	// price.computed is keyed by market (kafka/topics.go), and Delivery.MarketID
	// checks that declaration rather than trusting the string — which is the one
	// place in the pipeline where an identifier arrives untyped and could be
	// given back the wrong type.
	key, err := d.MarketID()
	if err != nil {
		return ApplyResult{Outcome: ApplyRejected},
			fmt.Errorf("%w: record at %s/%d offset %d: %w",
				ErrRecordRejected, d.Topic, d.Partition, d.Offset, err)
	}

	observedAt, _ := d.ObservedAt()

	if d.Tombstone {
		return s.removeLocked(key, observedAt), nil
	}

	if t := d.Envelope.Type; t != pricing.MessageType {
		// A frame problem, not a data problem: the type name versions the
		// PAYLOAD SHAPE independently of the envelope, so decoding a document
		// this build does not know as though it were one would misparse rather
		// than fail. internal/pricing says the same about its own input.
		return ApplyResult{Outcome: ApplyUnsupported, MarketID: key},
			fmt.Errorf("%w: %q at %s/%d offset %d (this build reads %q)",
				ErrUnsupportedMessage, t, d.Topic, d.Partition, d.Offset, pricing.MessageType)
	}

	var computed pricing.ComputedMarket
	if err := d.Unmarshal(&computed); err != nil {
		return ApplyResult{Outcome: ApplyRejected, MarketID: key},
			fmt.Errorf("%w: %w", ErrRecordRejected, err)
	}
	// Validate rather than trust. A record that has been on a compacted topic
	// for a month was written by an older build; running it back through the
	// document's own validator is what catches the case where it no longer
	// satisfies today's invariants — the argument normalizer/payload.go makes of
	// every consumer of these topics.
	if err := computed.Validate(); err != nil {
		return ApplyResult{Outcome: ApplyRejected, MarketID: key},
			fmt.Errorf("%w: %w", ErrRecordRejected, err)
	}

	payloadID, err := computed.MarketID()
	if err != nil {
		return ApplyResult{Outcome: ApplyRejected, MarketID: key},
			fmt.Errorf("%w: %w", ErrRecordRejected, err)
	}
	if payloadID != key {
		// The key routes the record and the payload identifies the market. When
		// they disagree, one of them is wrong and there is no way to tell which
		// — so the market would be delivered under one identifier and rendered
		// under another. Refused rather than resolved in favour of either.
		return ApplyResult{Outcome: ApplyRejected, MarketID: key},
			fmt.Errorf("%w: record at %s/%d offset %d is keyed %q but its payload names %q",
				ErrRecordRejected, d.Topic, d.Partition, d.Offset, key, payloadID)
	}

	channels, err := ChannelsFor(computed)
	if err != nil {
		// A market that cannot be routed to all three of its audiences is not
		// held at all; see ChannelsFor on why a partially routable market is
		// worse than none.
		return ApplyResult{Outcome: ApplyRejected, MarketID: key},
			fmt.Errorf("%w: %w", ErrRecordRejected, err)
	}

	if observedAt.IsZero() {
		// The envelope carried none, so fall back to the document's own
		// instant. Both are propagated unchanged from the provider, so they
		// agree in practice; the fallback exists because a record written before
		// the envelope carried the field still decodes, and treating it as
		// "observed at the zero time" would make every subsequent record for
		// that key look newer and would defeat the guard below in the one
		// direction it matters.
		observedAt = computed.ObservedAt
	}

	if prev, held := s.markets[key]; held && observedAt.Before(prev.ObservedAt) {
		// Monotonicity. price.computed is COMPACTED and this fold IS the
		// snapshot every connecting client receives, so applying an older
		// observation would publish a stale price as though it were news and
		// serve it to every subsequent connection. The normalizer and the pricer
		// take the same care against the same hazard for the same reason.
		//
		// An EQUAL instant is applied rather than skipped: a republication with
		// an unchanged observation instant is a real thing (the pricer
		// republishes a market when its configuration digest changes), and the
		// worst case of applying it is one duplicate delta carrying the full
		// market — which a client handles idempotently — against the far worse
		// case of suppressing a genuine change.
		return ApplyResult{Outcome: ApplyStale, MarketID: key, Channels: prev.Channels}, nil
	}

	market := Market{
		ID:         key,
		Payload:    bytes.Clone(d.Envelope.Data),
		Computed:   computed,
		Channels:   channels,
		ObservedAt: observedAt,
	}

	// Tear the old indexes down BEFORE installing the new ones, using the stored
	// channel set. A market whose event or league changed — a provider
	// re-keying, a market moved between leagues — would otherwise stay in the
	// old index for ever and keep being delivered to a board it had left.
	if prev, held := s.markets[key]; held {
		s.deindexLocked(prev)
	}
	s.markets[key] = market
	s.indexLocked(market)

	return ApplyResult{
		Outcome:  ApplyStored,
		MarketID: key,
		Market:   market,
		Channels: channels,
	}, nil
}

// removeLocked applies a tombstone. The caller holds the write lock.
func (s *Store) removeLocked(key domain.MarketID, observedAt time.Time) ApplyResult {
	prev, held := s.markets[key]
	if !held {
		// Nothing to delete and nothing to announce. Normal on a replay: the
		// compacted log retains a tombstone for delete.retention.ms after the
		// value it deleted has been collected, so a fresh reader meets the
		// deletion without ever having seen the market.
		return ApplyResult{Outcome: ApplyRemoved, MarketID: key}
	}
	if !observedAt.IsZero() && observedAt.Before(prev.ObservedAt) {
		// A deletion older than the state held would remove a market that has
		// since been re-published. Same guard, same reason, as the value path.
		return ApplyResult{Outcome: ApplyStale, MarketID: key, Channels: prev.Channels}
	}

	s.deindexLocked(prev)
	delete(s.markets, key)

	return ApplyResult{Outcome: ApplyRemoved, MarketID: key, Channels: prev.Channels}
}

// indexLocked adds a market to the secondary indexes. The caller holds the write
// lock.
func (s *Store) indexLocked(m Market) {
	for _, ch := range m.Channels {
		switch ch.Kind() {
		case ChannelEvent:
			id := domain.EventID(ch.ID())
			set, ok := s.byEvent[id]
			if !ok {
				set = make(map[domain.MarketID]struct{})
				s.byEvent[id] = set
			}
			set[m.ID] = struct{}{}
		case ChannelLeague:
			slug := domain.Slug(ch.ID())
			set, ok := s.byLeague[slug]
			if !ok {
				set = make(map[domain.MarketID]struct{})
				s.byLeague[slug] = set
			}
			set[m.ID] = struct{}{}
		case ChannelMarket:
			// The markets map IS the market index. A third map keyed by the same
			// identifier would be a second copy that could disagree with it.
		}
	}
}

// deindexLocked removes a market from the secondary indexes, and removes an
// index bucket that has become empty.
//
// Emptying the bucket is not tidiness. A league's markets are swept when its
// events settle, and a map that only ever grew would retain one entry per league
// and per event this replica has ever seen — which on a long-lived pod is a leak
// that presents as memory growth with no market to attribute it to.
func (s *Store) deindexLocked(m Market) {
	for _, ch := range m.Channels {
		switch ch.Kind() {
		case ChannelEvent:
			id := domain.EventID(ch.ID())
			if set, ok := s.byEvent[id]; ok {
				delete(set, m.ID)
				if len(set) == 0 {
					delete(s.byEvent, id)
				}
			}
		case ChannelLeague:
			slug := domain.Slug(ch.ID())
			if set, ok := s.byLeague[slug]; ok {
				delete(set, m.ID)
				if len(set) == 0 {
					delete(s.byLeague, slug)
				}
			}
		case ChannelMarket:
			// See indexLocked.
		}
	}
}

// Attach reads the snapshot for each channel and runs register in the SAME
// critical section.
//
// This is D2's subscribe half, and the reason the signature takes a callback
// instead of returning the snapshots. A caller that received a snapshot and then
// registered itself would leave a window — however small — in which a delta was
// published to a subscriber list this connection was not yet on, and the lost
// frame presents as a price that never moves again. Here the registration
// happens under the lock the publisher needs, so the window does not exist.
//
// register receives one entry per channel, INCLUDING channels with no markets:
// an empty snapshot is the correct answer for a league with no scheduled events
// and the client must be told so rather than left waiting. Zero-valued channels
// are skipped — they cannot have come from [ParseChannel], so their presence is
// a hub bug and inventing an answer for one would hide it.
//
// register runs under the WRITE lock even though the snapshot itself is a read.
// That is deliberate: register mutates the caller's routing table, and taking
// the read lock would let two registrations race each other and force a second
// mutex — reintroducing exactly the two-lock ordering problem this design
// removes. It must not block and must not call back into the Store.
func (s *Store) Attach(channels []Channel, register func(map[Channel][]Market)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapshots := make(map[Channel][]Market, len(channels))
	for _, ch := range channels {
		if ch.IsZero() {
			continue
		}
		snapshots[ch] = s.snapshotLocked(ch)
	}
	if register != nil {
		register(snapshots)
	}
}

// Exclusive runs fn under the same write lock Fold and Attach take.
//
// It is the escape hatch for the routing-table mutations that need the same
// ordering guarantee but no snapshot: an unsubscribe, a connection closing, a
// forced resync. Keeping them on this lock is what makes "the store's mutex is
// the one that orders publication against subscription" true of every path
// rather than of two of them.
//
// fn must not block and must not call back into the Store.
func (s *Store) Exclusive(fn func()) {
	if fn == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

// Snapshot returns the markets currently on a channel.
//
// It takes the READ lock and registers nothing, so it is the path for a
// client-requested resync — where the subscription already exists and only the
// content is being refreshed. A first subscription must go through [Attach]
// instead; using this and then registering would be the exact race D2 removes.
//
// A channel with no markets returns an EMPTY, non-nil slice. That is a correct
// answer and never an error: a league whose events have all settled genuinely
// has nothing on it, and nothing here fabricates a placeholder to make the
// result look populated.
func (s *Store) Snapshot(ch Channel) []Market {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked(ch)
}

// snapshotLocked is Snapshot's body. The caller holds the lock, in either mode.
//
// The result is ordered by market identifier. Map iteration order is randomised
// in Go, so an unordered snapshot would put the same markets on the wire in a
// different order on every connection — which makes a golden file impossible, a
// diff between two clients meaningless, and a rendering bug that depends on
// arrival order intermittent.
func (s *Store) snapshotLocked(ch Channel) []Market {
	switch ch.Kind() {
	case ChannelMarket:
		m, ok := s.markets[domain.MarketID(ch.ID())]
		if !ok {
			return []Market{}
		}
		return []Market{m}

	case ChannelEvent:
		return s.collectLocked(s.byEvent[domain.EventID(ch.ID())])

	case ChannelLeague:
		return s.collectLocked(s.byLeague[domain.Slug(ch.ID())])

	default:
		// Unreachable through ParseChannel, which admits three kinds. An empty
		// result rather than a panic (CLAUDE.md §12) — and empty rather than
		// invented, because a channel this build does not understand has no
		// markets on it by definition.
		return []Market{}
	}
}

// collectLocked materialises an index bucket, ordered.
func (s *Store) collectLocked(ids map[domain.MarketID]struct{}) []Market {
	out := make([]Market, 0, len(ids))
	for id := range ids {
		if m, ok := s.markets[id]; ok {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Market) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Len reports how many markets the slate holds.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.markets)
}

// MarketsTracked reports the same number as [Len].
//
// It exists as a separate name because it is the value
// sharpline_ws_markets_tracked carries, and `metrics.observeMarketsTracked(
// store.Len())` reads at the call site as though the gauge were about the length
// of something arbitrary. The gauge is a statement about the FOLD's completeness
// — every replica reads every partition, so a persistent disagreement between
// two replicas' values means one of them is not caught up — and the name is what
// carries that.
func (s *Store) MarketsTracked() int { return s.Len() }

// Stats is the slate's shape: markets held and how many distinct events and
// leagues they span.
type Stats struct {
	Markets int
	Events  int
	Leagues int
}

// Stats reports the slate's shape.
//
// The two index counts are the diagnostic that a market count alone cannot give:
// they are what shows an index bucket failing to be torn down (leagues climbing
// while markets are flat), which is the leak deindexLocked exists to prevent and
// which nothing else would surface.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{Markets: len(s.markets), Events: len(s.byEvent), Leagues: len(s.byLeague)}
}
