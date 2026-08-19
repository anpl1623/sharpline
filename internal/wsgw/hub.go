// The hub: the routing table, the fanout, and the two lifecycles it owns — the
// bus follower's and every connection's.
//
// Read doc.go first. It carries the arguments this file implements: D1 (the bus
// read is groupless, so the replay IS the snapshot), D2 (the state mutex is the
// linearisation point between snapshot and delta), D4 (backpressure removes the
// client's buffer, never the hub), D6 (Redis holds the durable subscription set
// and is still not the source of truth) and D7 (where the staleness SLO is
// observed, and why once per fanout event).
//
// # The hub has no mutex of its own
//
// Every piece of mutable state here — the routing table, the connection
// registry, the subscription count — is guarded by the STORE's write lock, and
// is reached through [Store.Attach] or [Store.Exclusive].
//
// That is not economy. A second mutex would have to be ordered against the
// store's, and the ordering would be the thing that makes D2 true or false: the
// snapshot is read under the store lock and the subscription is registered in
// the routing table, so if those were two locks then "no delta can be published
// between them" would depend on every future caller taking them in the same
// order. With one lock it is not a convention, it is arithmetic. state.go's
// Attach takes a CALLBACK for exactly this reason, and this file is the caller
// that shape was designed for.
//
// The one exception is `closed`, an atomic.Bool, because the upgrade handler
// reads it on every connection attempt and taking the fanout's write lock to
// answer "are we shutting down?" would put client arrivals on the same lock as
// price changes.
//
// # The lock order, stated once
//
//	store lock  ->  connection lock
//
// and never the reverse. The fanout runs under the store lock and hands frames
// to bounded queues, which takes each connection's lock briefly; nothing that
// holds a connection lock ever asks the store for anything. conn.go states the
// same rule from its side.
//
// # Redis is never on a locked path
//
// Every call into [Presence] happens OUTSIDE the store lock, and every one of
// them may fail without consequence to the socket (D6). A network round trip
// under the fanout lock would make an unreachable Redis into a fanout outage —
// which is precisely the coupling CLAUDE.md §3's "never the source of truth"
// exists to prevent.
package wsgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/wsgw/redispresence"
)

// -----------------------------------------------------------------------------
// Lifecycle errors
// -----------------------------------------------------------------------------
//
// These are about the HUB's lifecycle rather than about why a frame or a record
// was refused, which is what errors.go describes, so they are declared where
// they are used — the same split internal/pricing makes for ErrNotRunning and
// ErrNotRunnable.

var (
	// ErrNotRunning is what the readiness checker reports before [Hub.Run] has
	// started the bus follower and after it has stopped.
	ErrNotRunning = errors.New("wsgw: hub is not running")

	// ErrNotCaughtUp is what the readiness checker reports while the compacted
	// log is still being replayed.
	//
	// IT IS THE MOST IMPORTANT READINESS STATEMENT IN THIS SERVICE. A replica
	// that answered ready with a partial slate would accept connections and
	// serve snapshots that are missing markets — and nothing downstream could
	// tell, because an empty snapshot is a legitimate answer for a channel with
	// no markets (state.go). The client would subscribe successfully, see a
	// board with holes in it, and receive updates only for the markets that
	// happened to have been replayed. There is no metric that catches that and
	// no error anywhere; the only defence is refusing to be routed traffic
	// until the fold is complete.
	ErrNotCaughtUp = errors.New("wsgw: bus follower has not replayed the compacted log")

	// ErrNotRunnable is returned by [Hub.Run] when it was given no bus source.
	ErrNotRunnable = errors.New("wsgw: no bus source")

	// ErrShuttingDown is returned by [Hub.Register] once [Hub.Shutdown] has
	// begun. The upgrade handler turns it into a 503 (D11).
	ErrShuttingDown = errors.New("wsgw: gateway is shutting down")
)

// -----------------------------------------------------------------------------
// Seams
// -----------------------------------------------------------------------------

// BusSource is the part of *kafka.Follower the hub drives.
//
// Two methods, declared by the consumer (CLAUDE.md §12). The concrete type also
// carries a health check, a topic accessor and catch-up statistics; none of
// them is this package's business, and taking the whole type would make every
// test of the fanout require a broker.
//
// The follower's contract that matters here is D1's: no consumer group, direct
// partition assignment, reset to the start of the log, running on into the live
// tail without a seam. That is what makes replaying the compacted topic BE the
// snapshot (CLAUDE.md §3) and what lets every replica answer for every market —
// which CLAUDE.md §9's affinity-free routing requires and a consumer group
// cannot provide.
type BusSource interface {
	// Run delivers every record on the topic to h, from the beginning, until
	// ctx is cancelled. It blocks.
	Run(ctx context.Context, h kafka.Handler) error

	// HasCaughtUp reports whether the initial replay has completed. It is the
	// readiness gate; see [ErrNotCaughtUp].
	HasCaughtUp() bool
}

// Presence is the durable half of a client's subscription state (D6).
//
// It is six methods because the hub calls six, which is the correct reading of
// CLAUDE.md §12's "keep them small": small means no method the consumer does
// not use, not few methods. Each corresponds to one moment in a connection's
// life — restore, connect, subscribe, unsubscribe, heartbeat, disconnect — and
// dropping any of them would leave a key that nothing refreshes or nothing
// tears down.
//
// EVERY METHOD MAY FAIL WITHOUT CONSEQUENCE. A failure costs
// resume-on-reconnect and nothing else: the routing table that actually
// delivers frames is in the pod, and the CONTENT of a snapshot comes from the
// compacted Kafka topic and never from here. The hub counts each failure in
// sharpline_ws_presence_errors_total{op}, logs it at WARN (rate limited) and
// carries on serving the socket.
type Presence interface {
	// Channels returns the durable subscription set for a session, or an empty
	// slice when there is none. An absent session is normal, not an error.
	Channels(ctx context.Context, sessionKey string) ([]string, error)

	// Connected records that this replica now holds connectionID for a session
	// and refreshes the subscription set's lease.
	Connected(ctx context.Context, sessionKey, connectionID string, authenticated bool) error

	// Subscribe adds channels to the durable set.
	Subscribe(ctx context.Context, sessionKey string, channels []string, connectionID string, authenticated bool) error

	// Unsubscribe removes channels from it.
	Unsubscribe(ctx context.Context, sessionKey string, channels []string) error

	// Touch refreshes the leases. Called on every heartbeat.
	Touch(ctx context.Context, sessionKey, connectionID string) error

	// Disconnected removes the connection from fleet presence but KEEPS the
	// subscription set, which is the whole point: the set is what a reconnect
	// onto another replica restores from.
	Disconnected(ctx context.Context, sessionKey, connectionID string) error
}

// Compile-time proof that the shipped implementations satisfy the declarations.
// They are here rather than at the composition root because a mismatch should
// break THIS package's build, where the interfaces are declared.
var (
	_ BusSource     = (*kafka.Follower)(nil)
	_ Presence      = (*redispresence.Store)(nil)
	_ kafka.Handler = (*Hub)(nil)
	_ httpx.Checker = (*Hub)(nil)
	_ connHub       = (*Hub)(nil)
)

// noPresence is the default when no durable store is configured.
//
// It is a working configuration and not a degraded one: market data is public
// and comes from Kafka, so a gateway with no Redis serves every client
// correctly and loses only resume-on-reconnect. Making it the DEFAULT rather
// than requiring an explicit opt-out is what lets every unit test in this
// package run without a Redis, which is the difference between the fanout being
// tested and the fanout being tested occasionally.
type noPresence struct{}

func (noPresence) Channels(context.Context, string) ([]string, error) { return nil, nil }
func (noPresence) Connected(context.Context, string, string, bool) error {
	return nil
}
func (noPresence) Subscribe(context.Context, string, []string, string, bool) error { return nil }
func (noPresence) Unsubscribe(context.Context, string, []string) error             { return nil }
func (noPresence) Touch(context.Context, string, string) error                     { return nil }
func (noPresence) Disconnected(context.Context, string, string) error              { return nil }

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// DefaultPresenceTimeout bounds one call into [Presence].
//
// CLAUDE.md §12 puts a timeout on every external call, and this is the call
// site's own guarantee rather than a duplicate of the store's: redispresence
// bounds its commands too, but Presence is an interface anybody can implement
// and the hub must not depend on an implementation's diligence for a property
// it needs. Two seconds is far longer than a local Redis round trip and far
// shorter than a heartbeat interval, so a degrading Redis cannot make the
// heartbeat loop miss its next tick.
const DefaultPresenceTimeout = 2 * time.Second

// presenceWarnInterval rate-limits the WARN a failing presence store produces.
//
// Without it, a Redis outage costs one log line per heartbeat per client —
// which at the CLAUDE.md §10 target is five hundred lines a second saying the
// same thing, and the log becomes the second casualty of the outage.
const presenceWarnInterval = 30 * time.Second

// HubOptions are [NewHub]'s dependencies.
//
// It is separate from [Options] for the reason internal/pricing separates
// ServiceOptions from Options: one is a set of live collaborators and the other
// is a set of policy numbers, and a single struct carrying both would let a
// caller inject a Kafka follower and a ping interval in the same literal.
type HubOptions struct {
	// Options is the gateway's configuration. Required — its Logger is.
	Options Options

	// Source is the bus. Required for [Hub.Run]; a nil Source still builds a
	// usable hub, which is what the unit tests in this package use, and Run
	// then reports [ErrNotRunnable] rather than pretending to consume.
	Source BusSource

	// Presence is the durable subscription store (D6). Nil means [noPresence].
	Presence Presence

	// PresenceTimeout bounds one call into Presence. Zero means
	// [DefaultPresenceTimeout].
	PresenceTimeout time.Duration

	// Clock is the time source. Nil means time.Now. It exists so a test can
	// assert on a frame's `ts` without matching a moving value.
	Clock func() time.Time

	// NewConnectionID mints a connection identifier. Nil means a 128-bit random
	// value rendered as hex.
	//
	// It is injectable because the identifier appears in the `hello` frame and
	// in every log line for that connection, so a test that asserts on either
	// would otherwise have to match a random string.
	NewConnectionID func() string
}

// -----------------------------------------------------------------------------
// Hub
// -----------------------------------------------------------------------------

// Hub is the routing table and the fanout for one replica.
//
// It implements kafka.Handler (the bus side), httpx.Checker (the readiness
// side) and the connection side's [connHub], which is three roles for one type
// because all three are views of the same state and splitting them would mean
// splitting the lock that makes them consistent.
type Hub struct {
	opts     Options
	store    *Store
	source   BusSource
	presence Presence
	m        *Metrics
	log      *slog.Logger
	now      func() time.Time
	newID    func() string

	presenceTimeout time.Duration
	presenceWarn    warnLimiter

	// subs is the routing table: which live connections receive a frame on a
	// channel. GUARDED BY THE STORE'S WRITE LOCK — see the file comment.
	//
	// It is necessarily in the pod, because the thing it points at is a socket
	// and a socket exists in one process. D6's reconciliation of CLAUDE.md §9
	// and §3 turns on exactly that sentence.
	subs map[Channel]map[*conn]struct{}

	// conns is every live connection. Also guarded by the store's write lock.
	conns map[*conn]struct{}

	// subCount is the replica-wide subscription total behind
	// sharpline_ws_subscriptions_active. Maintained incrementally rather than
	// counted on demand, because "on demand" would mean walking the routing
	// table under the fanout lock on every subscribe.
	subCount int

	running atomic.Bool
	closed  atomic.Bool
}

// NewHub builds the hub. It performs no I/O and starts nothing.
//
// It returns an error rather than panicking (CLAUDE.md §12) and it validates
// eagerly, so a misconfigured gateway dies before it serves a connection rather
// than after it has served some.
func NewHub(opts HubOptions) (*Hub, error) {
	o := opts.Options.Normalise()
	if err := o.Validate(); err != nil {
		return nil, err
	}

	presence := opts.Presence
	if presence == nil {
		presence = noPresence{}
	}
	clock := opts.Clock
	if clock == nil {
		clock = time.Now
	}
	newID := opts.NewConnectionID
	if newID == nil {
		newID = randomID
	}
	if opts.PresenceTimeout < 0 {
		return nil, fmt.Errorf("%w: PresenceTimeout is %s", ErrInvalidOptions, opts.PresenceTimeout)
	}

	return &Hub{
		opts:            o,
		store:           NewStore(o.Metrics),
		source:          opts.Source,
		presence:        presence,
		m:               o.Metrics,
		log:             o.Logger.With(slog.String("component", "wsgw.hub")),
		now:             clock,
		newID:           newID,
		presenceTimeout: positiveDurationOr(opts.PresenceTimeout, DefaultPresenceTimeout),
		subs:            make(map[Channel]map[*conn]struct{}),
		conns:           make(map[*conn]struct{}),
	}, nil
}

// Options returns the normalised configuration, so the upgrade handler does not
// have to be given it twice and cannot be given a different copy.
func (h *Hub) Options() Options { return h.opts }

// Store returns the slate. It exists for the operational surface — a future
// admin endpoint reporting what this replica holds — and for the tests; nothing
// in the serving path reaches for it through this accessor.
func (h *Hub) Store() *Store { return h.store }

// Name implements httpx.Checker. It is the key this dependency appears under in
// the /readyz payload.
func (h *Hub) Name() string { return "stream" }

// Check implements httpx.Checker.
//
// Two conditions, and the second is the one that matters. The first says this
// process is consuming at all — a replica whose follower has exited but whose
// listener is still up would otherwise look healthy while fanning out nothing.
// The second says the fold is COMPLETE; see [ErrNotCaughtUp] for why serving a
// partial slate is worse than serving nothing.
func (h *Hub) Check(context.Context) error {
	if !h.running.Load() {
		return ErrNotRunning
	}
	if h.source != nil && !h.source.HasCaughtUp() {
		return ErrNotCaughtUp
	}
	return nil
}

// Connections reports how many live connections this replica holds.
func (h *Hub) Connections() int {
	n := 0
	h.store.Exclusive(func() { n = len(h.conns) })
	return n
}

// Run replays the compacted topic and then follows it live, until ctx is
// cancelled.
//
// It blocks for the life of the process, so a cmd/ entrypoint runs it in a
// goroutine alongside the httpx server and cancels the shared context on
// SIGTERM. It returns nil for a clean stop.
//
// There is no separate warm start and no snapshot phase. D1's follower reads
// the log from the beginning and continues into the live tail on the same poll
// loop, so the replay IS the slate and there is no handover to get wrong —
// which is the whole reason CLAUDE.md §3 chose a compacted topic.
func (h *Hub) Run(ctx context.Context) error {
	if h.source == nil {
		return ErrNotRunnable
	}

	h.log.Info("stream hub running",
		slog.String("consumes", kafka.TopicPriceComputed),
		slog.String("replica", h.opts.ReplicaID),
		slog.Int("send_queue_capacity", h.opts.SendQueueCapacity),
		slog.String("ping_interval", h.opts.PingInterval.String()),
	)

	h.running.Store(true)
	err := h.source.Run(ctx, h)
	h.running.Store(false)

	h.log.Info("stream hub bus follower stopped",
		slog.Int("markets_tracked", h.store.MarketsTracked()))
	return err
}

// HandleMessage folds one price.computed record into the slate and publishes
// the delta it produced, atomically (D2).
//
// It implements kafka.Handler.
//
// IT ALWAYS RETURNS NIL, and that is deliberate rather than sloppy. errors.go
// states the reason at ErrRecordRejected: a returned error stops the follower
// with the record undelivered, so one poison record on a COMPACTED topic would
// wedge the fanout for every client for ever and be redelivered for ever. A
// record that cannot be folded is counted in
// sharpline_ws_bus_records_total{result="rejected"} — which must be zero in a
// healthy system, so the failure is loud on the dashboard rather than in the
// process's exit code.
func (h *Hub) HandleMessage(_ context.Context, d *kafka.Delivery) error {
	res, err := h.store.Fold(d, h.publishLocked)
	if err != nil {
		h.log.Warn("skipping a record that could not be folded into the slate",
			slog.Any("delivery", d),
			slog.String("outcome", string(res.Outcome)),
			slog.String("error", err.Error()))
	}
	return nil
}

// publishLocked fans one applied record out to its subscribers.
//
// IT RUNS UNDER THE STORE'S WRITE LOCK. state.go's Fold hands the result here
// while still holding it, which is what makes "no delta can be published
// between reading a snapshot and registering the subscriber" true by
// construction. Everything in here must therefore be fast and must not block:
// it renders JSON and hands buffers to bounded queues, and it does nothing else.
//
// # The render is shared, the sequence number is not
//
// [Render] runs ONCE PER CHANNEL and [Body.Frame] runs once per connection.
// protocol.go argues why: at CLAUDE.md §10's ten thousand subscribers, a delta
// on a popular market marshalled per connection is the single decision that
// makes the target impossible. It is per channel rather than per record because
// the frame names the channel it is delivered on, so `market:x`, `event:y` and
// `league:nfl` are three bodies — and three is the constant, not N.
//
// # The staleness observation (D7)
//
// [Metrics.observeFanout] is called ONCE, and only when at least one client
// actually received the record. Once per subscriber would put thousands of
// identical samples into the histogram and weight the headline SLO by market
// POPULARITY rather than by freshness; a record nobody is subscribed to is not
// a fanout event at all, and counting it would make the SLO a statement about
// the bus instead of about the clients. metrics.go states both halves at
// length.
//
// A tombstone is deliberately NOT observed: it carries no prices and no
// ingested_at, so there is no age to measure, and inventing one from the
// delivery's own timestamp would put a number into the SLO that no price ever
// had.
func (h *Hub) publishLocked(res ApplyResult) {
	now := h.now()

	switch res.Outcome {
	case ApplyStored:
		sent := h.fanoutLocked(res.Channels, now, func(ch Channel) FrameBody {
			return NewDelta(ch, res.Market.Payload)
		})
		if sent > 0 {
			h.m.observeSentN(KindDelta, sent)
			h.m.observeFanout(res.Market.Computed, now)
		}

	case ApplyRemoved:
		id := res.MarketID.String()
		sent := h.fanoutLocked(res.Channels, now, func(ch Channel) FrameBody {
			return NewRemoval(ch, id)
		})
		if sent > 0 {
			h.m.observeSentN(KindDelta, sent)
		}

	case ApplyStale, ApplyRejected, ApplyUnsupported:
		// Nothing changed, so there is nothing to announce. state.go only calls
		// this callback for the two outcomes above; the branch exists so that
		// adding an outcome breaks here rather than silently publishing nothing.
	}
}

// fanoutLocked renders one body per channel and queues it to every subscriber,
// returning how many connections received it. The caller holds the write lock.
func (h *Hub) fanoutLocked(channels []Channel, at time.Time, build func(Channel) FrameBody) int {
	sent := 0
	for _, ch := range channels {
		subs := h.subs[ch]
		if len(subs) == 0 {
			continue
		}
		body, err := Render(build(ch), at, "")
		if err != nil {
			// Unrenderable means a Channel that never came from ParseChannel or
			// a market document that will not marshal — a bug on this side. The
			// other channels still go out; suppressing them too would turn one
			// malformed frame into a silent board.
			h.log.Error("dropping an unrenderable delta",
				slog.String("channel", ch.String()),
				slog.String("error", err.Error()))
			continue
		}
		for c := range subs {
			c.enqueue(body, KindDelta)
			sent++
		}
	}
	return sent
}

// -----------------------------------------------------------------------------
// Registration
// -----------------------------------------------------------------------------

// Register admits an upgraded connection: it restores the durable subscription
// set, sends the `hello` frame and the snapshots that go with it, and puts the
// connection in the routing table.
//
// The ORDER is the contract. The restore is I/O and happens outside the lock;
// the hello frame and every restored channel's snapshot are enqueued INSIDE one
// [Store.Attach] critical section, so the connection cannot miss a delta
// published between "your channels are back" and "you are subscribed to them"
// (D2). The presence write that records the connection happens afterwards,
// because it is I/O and because its failure must not stop a connection that is
// already correct.
func (h *Hub) Register(ctx context.Context, c *conn) error {
	if h.closed.Load() {
		return ErrShuttingDown
	}

	restored := h.restore(ctx, c)

	var admitted bool
	h.store.Attach(restored, func(snapshots map[Channel][]Market) {
		if h.closed.Load() {
			return
		}
		admitted = true
		h.conns[c] = struct{}{}

		c.sendFrame(NewHello(
			c.id,
			c.sessionID,
			h.opts.PingInterval,
			len(restored) > 0,
			!c.identity.Anonymous(),
			restored,
		), "")

		for _, ch := range restored {
			if !c.addLocked(ch) {
				continue
			}
			h.addRouteLocked(ch, c)
			c.sendFrame(NewSnapshot(ch, marketPayloads(snapshots[ch])), "")
		}
		h.observeSubscriptionsLocked()
	})

	if !admitted {
		return ErrShuttingDown
	}

	h.presenceOp(ctx, PresenceOpHeartbeat, func(ctx context.Context) error {
		return h.presence.Connected(ctx, c.sessionID, c.id, !c.identity.Anonymous())
	})

	h.log.Info("connection registered",
		slog.Any("connection", c),
		slog.Int("restored_channels", len(restored)),
	)
	return nil
}

// restore reads a session's durable subscription set (D6).
//
// It returns an EMPTY set on every failure, and that is the whole degradation
// policy in one line: a client whose channels could not be restored gets a
// correct, empty `hello` and re-lists them, which costs one round trip. The
// alternative — refusing the connection because Redis is unavailable — would
// trade a degraded convenience for a total outage of a surface CLAUDE.md §6
// makes public.
//
// A stored channel that no longer parses is dropped rather than refused. It
// cannot have come from this build, so the honest reading is that the grammar
// changed under a set that outlived it, and the client re-subscribes.
func (h *Hub) restore(ctx context.Context, c *conn) []Channel {
	if !c.resumable || c.sessionID == "" {
		return nil
	}

	var names []string
	h.presenceOp(ctx, PresenceOpRestore, func(ctx context.Context) error {
		var err error
		names, err = h.presence.Channels(ctx, c.sessionID)
		return err
	})
	if len(names) == 0 {
		return nil
	}

	out := make([]Channel, 0, len(names))
	for _, name := range names {
		ch, err := ParseChannel(name)
		if err != nil {
			h.log.Warn("dropping a restored channel this build cannot parse",
				slog.String("channel", SafeEcho(name)),
				slog.String("reason", string(ChannelRejectReason(name))))
			continue
		}
		out = append(out, ch)
		if len(out) == h.opts.MaxChannelsPerConnection {
			// The cap is a refusal, not a truncation, for a subscribe — but a
			// restore is not a client request, and a stored set larger than the
			// cap can only mean the cap was lowered. Refusing the whole
			// connection over an operator's own configuration change would be
			// the wrong party to punish.
			h.log.Warn("restored subscription set exceeds the per-connection cap; truncating",
				slog.Int("cap", h.opts.MaxChannelsPerConnection),
				slog.Int("stored", len(names)))
			break
		}
	}
	sortChannels(out)
	return out
}

// unregister removes a connection from the routing table and from fleet
// presence. It implements [connHub] and is called exactly once, by
// [conn.serve], after every loop that connection owned has stopped.
func (h *Hub) unregister(ctx context.Context, c *conn) {
	h.store.Exclusive(func() {
		if _, ok := h.conns[c]; !ok {
			return
		}
		delete(h.conns, c)
		for _, ch := range c.channelsLocked() {
			h.removeRouteLocked(ch, c)
			c.removeLocked(ch)
		}
		h.observeSubscriptionsLocked()
	})

	// The caller's context is already cancelled by the time serve reaches here,
	// and the teardown still has to happen — the same reason internal/pricing
	// flushes its producer on a detached context. Without this, every clean
	// disconnect would leave a stale entry in the fleet presence set until its
	// TTL expired.
	toCtx := context.WithoutCancel(ctx)
	h.presenceOp(toCtx, PresenceOpForget, func(ctx context.Context) error {
		return h.presence.Disconnected(ctx, c.sessionID, c.id)
	})
}

// -----------------------------------------------------------------------------
// Client requests
// -----------------------------------------------------------------------------

// subscribe adds channels to a connection and answers with an ack followed by
// one snapshot per accepted channel. It implements [connHub].
//
// # Why this is two critical sections and not one
//
// Classification — which channels are new, which are duplicates, which would
// exceed the cap — is done first, under [Store.Exclusive]. Only the survivors
// go into [Store.Attach], which SNAPSHOTS EVERY CHANNEL IT IS GIVEN before
// running the callback. Doing both in one Attach would snapshot channels that
// are about to be rejected, and a client sending 256 league channels would make
// this replica materialise 256 league snapshots under the fanout lock before
// throwing 255 of them away. The classification pass is a map lookup per
// channel and reserves nothing, which is safe because a connection's
// subscription set is mutated only by its own read loop.
//
// # Refusals are per channel and are never a truncation
//
// A subscribe naming three good channels and one bad one produces three
// subscriptions and one bounded rejection. Rounding that to success would leave
// the client believing it holds a channel it does not; rounding it to failure
// would throw away the three that were fine. options.go states the rule for
// every limit in this package.
func (h *Hub) subscribe(ctx context.Context, c *conn, raw []string, requestID string) {
	parsed, rejected := h.parseChannels(raw)

	var accepted []Channel
	h.store.Exclusive(func() {
		room := h.opts.MaxChannelsPerConnection - c.countLocked()
		for i, ch := range parsed {
			switch {
			case c.holdsLocked(ch) || slices.Contains(accepted, ch):
				rejected = append(rejected, RejectedChannel{
					Channel: SafeEcho(raw[i]), Reason: RejectDuplicate,
				})
				h.m.observeChannelReject(RejectDuplicate)
			case len(accepted) >= room:
				rejected = append(rejected, RejectedChannel{
					Channel: SafeEcho(raw[i]), Reason: RejectLimitReached,
				})
				h.m.observeChannelReject(RejectLimitReached)
			default:
				accepted = append(accepted, ch)
			}
		}
	})

	h.store.Attach(accepted, func(snapshots map[Channel][]Market) {
		c.sendFrame(NewAck(accepted, rejected), requestID)
		for _, ch := range accepted {
			if !c.addLocked(ch) {
				continue
			}
			h.addRouteLocked(ch, c)
			c.sendFrame(NewSnapshot(ch, marketPayloads(snapshots[ch])), requestID)
		}
		h.observeSubscriptionsLocked()
	})

	if len(accepted) == 0 {
		return
	}
	names := channelNames(accepted)
	h.presenceOp(ctx, PresenceOpSubscribe, func(ctx context.Context) error {
		return h.presence.Subscribe(ctx, c.sessionID, names, c.id, !c.identity.Anonymous())
	})
}

// unsubscribe removes channels and answers with an ack. It implements
// [connHub].
//
// The routing entry is removed inside the store's critical section, so the ack
// is the last thing the client receives for that channel: a delta cannot be
// published to a subscriber list this connection has already left.
//
// A channel the connection does not hold is a NO-OP rather than a rejection.
// [RejectReason] has no value that means "you never held this", and coercing it
// into `duplicate` would put a wrong classification into a Prometheus label to
// avoid leaving a gap in a frame. redispresence states the same rule for the
// same situation — "removing a channel the session does not hold is a no-op,
// not an error: an unsubscribe racing an expiry is normal" — and an unsubscribe
// racing a forced resync is the local version of that race. Parse failures ARE
// still reported, because those are a client mistake with a reason that fits.
func (h *Hub) unsubscribe(ctx context.Context, c *conn, raw []string, requestID string) {
	parsed, rejected := h.parseChannels(raw)

	var removed []Channel
	h.store.Exclusive(func() {
		for _, ch := range parsed {
			if !c.removeLocked(ch) {
				continue
			}
			h.removeRouteLocked(ch, c)
			removed = append(removed, ch)
		}
		h.observeSubscriptionsLocked()
		c.sendFrame(NewAck(removed, rejected), requestID)
	})

	if len(removed) == 0 {
		return
	}
	names := channelNames(removed)
	h.presenceOp(ctx, PresenceOpUnsubscribe, func(ctx context.Context) error {
		return h.presence.Unsubscribe(ctx, c.sessionID, names)
	})
}

// resync re-sends a snapshot for the named channels. It implements [connHub].
//
// An EMPTY channel list means every channel this connection holds, which is
// what a client sends after detecting a sequence gap — at that point it knows
// something was lost but not which channel lost it.
//
// # Why this takes the WRITE lock, when state.go offers a read-locked Snapshot
//
// [Store.Snapshot] reads under the read lock and is the obvious choice here:
// the subscription already exists and only the content is being refreshed. It
// is not enough. Between "read the snapshot" and "queue the snapshot frame" a
// delta could be published and queued FIRST, so the client would apply the
// delta and then overwrite it with the older snapshot — a price that silently
// moves backwards, which is worse than the gap the resync was answering.
// [Store.Attach] closes that window by doing both under one lock, and a
// re-registration of a channel the connection already holds is a no-op. The
// cost is that a resync storm serialises against the fanout, which is the
// correct trade: resyncs are rare by design and are alerted on when they are
// not (WebSocketResyncStorm).
func (h *Hub) resync(_ context.Context, c *conn, raw []string, requestID string, reason ResyncReason) {
	var targets []Channel

	if len(raw) == 0 {
		h.store.Exclusive(func() { targets = c.channelsLocked() })
	} else {
		parsed, rejected := h.parseChannels(raw)
		h.store.Exclusive(func() {
			for _, ch := range parsed {
				// A channel this connection does not hold is skipped rather
				// than refused, for the reason unsubscribe gives: there is no
				// RejectReason that says "you never held this", and a wrong
				// bounded classification is worse than a quiet no-op.
				if !c.holdsLocked(ch) {
					continue
				}
				targets = append(targets, ch)
			}
			if len(rejected) > 0 {
				c.sendFrame(NewAck(targets, rejected), requestID)
			}
		})
	}

	if len(targets) == 0 {
		// Nothing was resynchronised, so nothing is counted. The metric is
		// "clients told to resynchronise"; a request against an empty
		// subscription set told nobody anything, and counting it would put
		// noise into WebSocketResyncStorm's numerator.
		return
	}
	h.m.observeResync(reason)

	h.store.Attach(targets, func(snapshots map[Channel][]Market) {
		for _, ch := range targets {
			c.sendFrame(NewSnapshot(ch, marketPayloads(snapshots[ch])), requestID)
		}
	})
}

// heartbeat refreshes the durable subscription set's lease. It implements
// [connHub] and runs on the connection's ping goroutine, after a control-frame
// round trip has proved the socket is alive.
//
// Tying the lease to evidence of liveness rather than to a timer is what stops
// a half-open connection from holding a session's channels open in Redis for
// the whole TTL after the client is gone.
func (h *Hub) heartbeat(ctx context.Context, c *conn) {
	if c.sessionID == "" {
		return
	}
	h.presenceOp(ctx, PresenceOpHeartbeat, func(ctx context.Context) error {
		return h.presence.Touch(ctx, c.sessionID, c.id)
	})
}

// parseChannels turns client strings into channels, collecting bounded
// rejections for the ones that fail.
//
// The returned slices are index-aligned with the input only for the accepted
// half; a rejected entry carries its own echoed string, because a client that
// sent forty channels needs to know WHICH one was refused and the ack is the
// only place it can be told.
func (h *Hub) parseChannels(raw []string) ([]Channel, []RejectedChannel) {
	parsed := make([]Channel, 0, len(raw))
	var rejected []RejectedChannel
	for _, s := range raw {
		ch, err := ParseChannel(s)
		if err != nil {
			reason := ChannelRejectReason(s)
			rejected = append(rejected, RejectedChannel{Channel: SafeEcho(s), Reason: reason})
			h.m.observeChannelReject(reason)
			continue
		}
		parsed = append(parsed, ch)
	}
	return parsed, rejected
}

// -----------------------------------------------------------------------------
// Routing table
// -----------------------------------------------------------------------------
//
// Both mutators run under the store's write lock. Neither takes one of its own,
// deliberately: a lock here would suggest the table can be read outside that
// section, and a read of the routing table outside it is the race D2 removes.

// addRouteLocked records that c receives frames on ch.
func (h *Hub) addRouteLocked(ch Channel, c *conn) {
	subs, ok := h.subs[ch]
	if !ok {
		subs = make(map[*conn]struct{})
		h.subs[ch] = subs
	}
	if _, held := subs[c]; !held {
		subs[c] = struct{}{}
		h.subCount++
	}
}

// removeRouteLocked stops delivering ch to c, and removes a channel bucket that
// has become empty.
//
// Emptying the bucket is not tidiness. A gateway that only ever added map
// entries would retain one per channel any client has ever subscribed to, for
// the life of the pod — which on a service whose clients subscribe to
// per-market channels is a leak that grows with the size of the board and has
// no market to attribute it to. state.go tears its own index buckets down for
// exactly this reason.
func (h *Hub) removeRouteLocked(ch Channel, c *conn) {
	subs, ok := h.subs[ch]
	if !ok {
		return
	}
	if _, held := subs[c]; held {
		delete(subs, c)
		h.subCount--
	}
	if len(subs) == 0 {
		delete(h.subs, ch)
	}
}

// observeSubscriptionsLocked publishes the replica-wide subscription count.
func (h *Hub) observeSubscriptionsLocked() { h.m.observeSubscriptions(h.subCount) }

// -----------------------------------------------------------------------------
// Shutdown (D11)
// -----------------------------------------------------------------------------

// Shutdown stops accepting upgrades, tells every live connection the server is
// going away, gives them a bounded moment to receive it, and then closes them.
//
// The ORDER is the point, and each step buys something:
//
//  1. `closed` is set first, so no connection can be admitted into a hub that
//     is already tearing down and then be missed by the loop below.
//  2. Every connection is sent a coded `error` frame with CodeGoingAway. It
//     goes through the normal queue and therefore carries a sequence number,
//     so a client that reconnects can tell a deliberate shutdown from a socket
//     that simply died — which is the difference between reconnecting
//     immediately and backing off.
//  3. The drain gives the writers up to Options.ShutdownDrain to flush, and
//     ends the moment every queue is empty. deploy's compose gives this
//     service stop_grace_period: 20s and Kubernetes' default
//     terminationGracePeriodSeconds is 30, so the default of 5s leaves the rest
//     of the budget to the bus follower and the listener — but spending all of
//     it when there is nothing left to write would make every deploy five
//     seconds slower for no reason, which is how a drain budget gets lowered
//     until it stops working.
//  4. Only then is each connection closed with a going-away status, counted as
//     shutdown rather than as a client fault — a deploy must not read as a
//     fanout regression on the dashboard.
//
// It is idempotent, and safe to call while [Hub.Run] is still returning.
func (h *Hub) Shutdown(ctx context.Context) error {
	if !h.closed.CompareAndSwap(false, true) {
		return nil
	}

	var live []*conn
	h.store.Exclusive(func() {
		live = make([]*conn, 0, len(h.conns))
		for c := range h.conns {
			live = append(live, c)
		}
	})
	if len(live) == 0 {
		h.log.Info("gateway shutting down with no live connections")
		return nil
	}

	h.log.Info("gateway shutting down",
		slog.Int("connections", len(live)),
		slog.String("drain", h.opts.ShutdownDrain.String()))

	for _, c := range live {
		c.sendFrame(NewError(CodeGoingAway, protocolErrorText(CodeGoingAway)), "")
	}

	h.drain(ctx, live)

	for _, c := range live {
		c.requestClose(DropShutdown, websocket.StatusGoingAway, "server going away")
	}

	for _, c := range live {
		select {
		case <-c.wait():
		case <-ctx.Done():
			// The orchestrator's grace period is about to expire. Say which
			// connections are still open rather than exiting silently: a
			// straggler here means a client whose socket will be reset rather
			// than closed, and knowing how many is the difference between a
			// tuning problem and a leak.
			h.log.Warn("shutdown budget expired with connections still draining",
				slog.String("connection_id", c.id))
			return ctx.Err()
		}
	}

	h.log.Info("every connection closed")
	return nil
}

// -----------------------------------------------------------------------------
// Presence plumbing
// -----------------------------------------------------------------------------

// The `op` label values are metrics.go's CLOSED SET — restore, subscribe,
// unsubscribe, heartbeat, forget — and this file emits no others. Two of the
// hub's six calls do not have a label of their own, and each is mapped rather
// than given one:
//
//	Presence.Connected     -> PresenceOpHeartbeat. It writes the session hash
//	                          and refreshes every lease, which is what a
//	                          heartbeat does; it is the FIRST heartbeat, taken
//	                          at the instant the connection is admitted.
//	Presence.Disconnected  -> PresenceOpForget. That value's doc calls it
//	                          "tearing the session down on close", which is this
//	                          moment. The gateway deliberately calls
//	                          Disconnected rather than Forget, because D6 keeps
//	                          the subscription set alive so a reconnect onto
//	                          another replica can resume from it — the label
//	                          names the moment, not the Redis command.
//
// Inventing two new values would have been more precise and would have broken
// the set: PresenceOp.Valid and PresenceOps() live in metrics.go and would
// reject them, and a label value that its own type says is invalid is worse
// than a slightly coarse one.

// presenceOp runs one call into the durable store, bounded, and turns every
// failure into a counter and a rate-limited log line.
//
// Nothing it does can fail a connection. D6 states the degradation policy per
// case and this function is where all three cases land: unreachable at connect
// means the set is pod-local for this socket's life, unreachable mid-life means
// the lease stops being refreshed, and a wiped keyspace means clients re-list
// their channels on their next reconnect. In every one of them the socket is
// untouched.
func (h *Hub) presenceOp(ctx context.Context, op PresenceOp, fn func(context.Context) error) {
	if h.presence == nil {
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, h.presenceTimeout)
	defer cancel()

	err := fn(callCtx)
	if err == nil {
		return
	}
	h.m.observePresenceError(op)
	if h.presenceWarn.allow(h.now(), presenceWarnInterval) {
		h.log.Warn("subscription state operation failed; resume-on-reconnect is degraded",
			slog.String("op", string(op)),
			slog.String("error", err.Error()))
	}
}

// shutdownPollInterval is how often the drain checks whether it can stop early.
//
// It is short relative to any plausible ShutdownDrain, because the common case
// is that every queue empties within a millisecond and the whole drain should
// cost about that. It is not zero, because a spin loop over ten thousand
// connections is a worse way to wait than a sleep.
const shutdownPollInterval = 10 * time.Millisecond

// drain waits for every connection's send queue to empty, bounded by
// Options.ShutdownDrain and by ctx.
//
// It watches the QUEUES rather than sleeping for the full budget, so a shutdown
// with nothing pending returns immediately. A connection that has already begun
// closing counts as drained: its writer is finishing on its own and nothing
// more will be queued to it.
func (h *Hub) drain(ctx context.Context, live []*conn) {
	deadline := h.now().Add(h.opts.ShutdownDrain)
	ticker := time.NewTicker(shutdownPollInterval)
	defer ticker.Stop()

	for {
		pending := 0
		for _, c := range live {
			if !c.closing() {
				pending += len(c.send)
			}
		}
		if pending == 0 {
			return
		}
		select {
		case <-ticker.C:
			if h.now().After(deadline) {
				h.log.Warn("shutdown drain expired with frames still queued",
					slog.Int("queued", pending))
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// warnLimiter admits one log line per interval.
//
// It is a compare-and-swap on a Unix-nanosecond stamp rather than a mutex,
// because it is consulted on the heartbeat path of every connection: at
// CLAUDE.md §10's ten thousand subscribers a shared mutex here would be
// contention introduced purely to decide whether to log.
type warnLimiter struct{ last atomic.Int64 }

// allow reports whether a line may be emitted now.
func (w *warnLimiter) allow(now time.Time, every time.Duration) bool {
	n := now.UnixNano()
	for {
		prev := w.last.Load()
		if prev != 0 && n-prev < int64(every) {
			return false
		}
		if w.last.CompareAndSwap(prev, n) {
			return true
		}
	}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// sortChannels orders channels by kind and then by identifier.
//
// Ordered rather than left to map iteration for the reason state.go orders its
// snapshots: Go randomises map order, so an unordered set puts the same
// channels on the wire in a different order on every connection, which makes a
// golden file impossible and a rendering bug that depends on arrival order
// intermittent.
func sortChannels(chans []Channel) {
	slices.SortFunc(chans, func(a, b Channel) int {
		if k := strings.Compare(string(a.Kind()), string(b.Kind())); k != 0 {
			return k
		}
		return strings.Compare(a.ID(), b.ID())
	})
}

// channelNames renders channels for the durable store, which holds NAMES and
// nothing derived from a price (D6).
func channelNames(chans []Channel) []string {
	out := make([]string, 0, len(chans))
	for _, ch := range chans {
		out = append(out, ch.String())
	}
	return out
}

// randomID mints a 128-bit identifier, rendered as hex.
//
// crypto/rand rather than math/rand, and 128 bits rather than 64, because the
// value is a connection id that appears in the `hello` frame and — for an
// anonymous client — is also the session key its durable subscription set is
// stored under. A guessable session key would let a stranger read which
// channels somebody is watching. crypto/rand.Read cannot fail on any platform
// this runs on, and the error is nonetheless handled rather than ignored: a
// zero-valued identifier would collide with every other, so the fallback is a
// value that is unique for a different reason.
func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// positiveDurationOr resolves a non-positive duration to its default.
func positiveDurationOr(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
