// The gateway's configuration: the bounded queue, the channel and frame caps,
// the heartbeat and the shutdown drain.
//
// Every value here is a REFUSAL threshold, not a truncation threshold, and that
// distinction is the reason the file has a doc comment at all. A gateway that
// silently trims an over-long subscribe list, or quietly drops the tail of an
// over-long frame, produces a client that believes it asked for something it did
// not get — and the failure surfaces days later as "that market stopped
// updating", with nothing in any log connecting the two. Everything below either
// succeeds completely or is refused with a coded reason.
package wsgw

import (
	"fmt"
	"log/slog"
	"time"
)

// Defaults. Zero means the default in every case; a NEGATIVE value is always a
// configuration error rather than a disable, because nothing here is safe to
// switch off (the httpx server's negative-means-no-deadline convention applies
// to write deadlines on the listener, which is a different question — see
// [Options.WriteTimeout]).
const (
	// DefaultSendQueueCapacity is the per-connection bounded send queue (D4).
	//
	// 256 frames is roughly two seconds of a busy league channel at the ADR 0003
	// live cadence, which is the right order: long enough that a client pausing
	// for a garbage collection or a tab switch recovers without a resync, short
	// enough that 10k connections at the CLAUDE.md §10 target cost megabytes of
	// queued pointers rather than gigabytes of queued frames. Raising it does
	// not make a slow client fast; it makes the resync arrive later and the
	// memory bill larger.
	DefaultSendQueueCapacity = 256

	// DefaultMaxChannelsPerConnection caps how much of the board one connection
	// may hold (D9).
	//
	// It is a fanout-cost control, not a memory one: every channel a connection
	// holds is a routing-table entry the publish path walks, so an unbounded
	// subscriber makes every OTHER client's delta slower. 256 comfortably
	// covers the widest legitimate client — a board page holding a handful of
	// leagues, an event page holding one event, a slip holding a few markets.
	DefaultMaxChannelsPerConnection = 256

	// DefaultMaxFrameBytes caps one inbound frame.
	//
	// The largest legitimate client frame is a subscribe carrying
	// MaxChannelsPerFrame channels at MaxChannelLen each, which is under 35 KiB;
	// 64 KiB leaves room for that plus the envelope without admitting a frame
	// large enough to be interesting as a memory-pressure lever. Nothing a
	// client sends is ever larger, because clients send requests, not data.
	DefaultMaxFrameBytes = 64 << 10

	// DefaultPingInterval is how often the server sends an RFC 6455 control
	// ping (D10).
	//
	// 20 seconds is chosen against the proxy layer rather than against the
	// clients: idle-connection timeouts in front of this service (Caddy here, an
	// Ingress controller in the cluster, and whatever CDN or corporate proxy sits
	// between it and a browser) commonly start at 60 seconds, and a keepalive at
	// a third of that survives one lost ping without the connection being reaped
	// by a middlebox that thinks it is dead.
	DefaultPingInterval = 20 * time.Second

	// DefaultPongTimeout is how long a connection has to answer a ping before it
	// is reaped as idle (D10).
	//
	// It must be comfortably below PingInterval so a reap decision is made from
	// ONE outstanding ping. If it were longer, two pings could be in flight and
	// the timeout would be measuring something other than what it says.
	DefaultPongTimeout = 10 * time.Second

	// DefaultWriteTimeout bounds ONE socket write.
	//
	// It is emphatically NOT the listener's write deadline: httpx's
	// ServerOptions.WriteTimeout must be NEGATIVE for this service, because a
	// deadline on the whole response severs a long-lived stream — httpx/health.go
	// names `stream` as the service that needs that. This is the per-write
	// budget the connection loop applies instead, and its job is to convert a
	// TCP peer that has stopped reading into a bounded failure rather than a
	// goroutine parked for ever holding a send queue.
	DefaultWriteTimeout = 10 * time.Second

	// DefaultShutdownDrain is how long every live connection is given to receive
	// its going-away close frame before the process stops (D11).
	//
	// deploy/compose gives `stream` stop_grace_period: 20s, and Kubernetes'
	// default terminationGracePeriodSeconds is 30. Five seconds leaves the rest
	// of the budget to the bus follower and the operational listener, and it is
	// far more than a close frame needs — the point of the drain is that clients
	// learn to reconnect elsewhere rather than discover a dead socket.
	DefaultShutdownDrain = 5 * time.Second

	// DefaultSubscriptionTTL is how long a durable subscription set survives in
	// Redis without a heartbeat (D6).
	//
	// It has to exceed the reconnect window a client with a jittered backoff
	// actually uses (CLAUDE.md §7) — otherwise a resume that the design promises
	// works on a fast reconnect and silently does not on a slow one, which is the
	// worst of both. Five minutes covers a laptop lid closing briefly; anything
	// longer starts to accumulate sets for sessions that are never coming back,
	// and Redis is not the source of truth for any of it (D6), so expiring them
	// costs a client one re-subscribe.
	DefaultSubscriptionTTL = 5 * time.Minute
)

// Options configures the gateway.
//
// Constructor-injected, never read from the environment: internal/platform/config
// loads the process's configuration once and hands it down (CLAUDE.md §12). A
// zero value is not usable — [Options.Validate] refuses it — because Logger is
// required and a service that cannot log its own connections is not observable.
type Options struct {
	// Logger receives connection lifecycle events. Required.
	Logger *slog.Logger

	// Metrics is the collector set. Optional: a nil Metrics makes every observe
	// call a no-op, which is right for a unit test and for any process with no
	// /metrics endpoint. Nothing in this package needs a nil check because of it.
	Metrics *Metrics

	// Verifier verifies a presented access token. Optional.
	//
	// Nil means NO token can be accepted — not that tokens are ignored.
	// Anonymous connections still work, because market data is public (D5), but
	// a client that presents a credential to a gateway with no verifier is
	// REFUSED rather than downgraded. A silent downgrade is how a deployment
	// that forgot to configure a signing key serves authenticated-looking
	// traffic to everyone.
	Verifier TokenVerifier

	// ReplicaID names this pod in the Redis presence set (D6). Empty is legal —
	// presence is then keyed by the connection alone and the fleet view loses
	// the per-replica breakdown, which degrades an operator's diagnosis rather
	// than the service.
	ReplicaID string

	// SendQueueCapacity is the per-connection bounded queue (D4).
	SendQueueCapacity int

	// MaxChannelsPerConnection caps the subscription set (D9). A subscribe that
	// would exceed it is REFUSED, per channel, with RejectLimitReached — never
	// truncated to the first N.
	MaxChannelsPerConnection int

	// MaxFrameBytes caps one inbound frame (D9). An over-long frame is REFUSED
	// with ErrFrameTooLarge — never truncated, because a truncated JSON document
	// either fails to parse or, worse, parses into something the client did not
	// send.
	MaxFrameBytes int

	// PingInterval and PongTimeout are the heartbeat (D10). PongTimeout must be
	// strictly less than PingInterval; see DefaultPongTimeout.
	PingInterval time.Duration
	PongTimeout  time.Duration

	// WriteTimeout bounds one socket write. See DefaultWriteTimeout on why this
	// is not the listener's write deadline.
	WriteTimeout time.Duration

	// ShutdownDrain is the going-away budget (D11).
	ShutdownDrain time.Duration

	// SubscriptionTTL is the Redis key lifetime for a durable subscription set
	// (D6), refreshed on every heartbeat.
	SubscriptionTTL time.Duration
}

// Normalise returns a copy of o with every zero-valued knob replaced by its
// default.
//
// It is a separate step from [Options.Validate] so that Validate can tell a
// DELIBERATE bad value from an unset one: -1 is a configuration mistake worth
// failing startup over, while 0 is "I did not have an opinion". Merging the two
// would make a negative value silently become a default, which is how a typo in
// a Helm values file becomes a production setting nobody chose.
func (o Options) Normalise() Options {
	if o.SendQueueCapacity == 0 {
		o.SendQueueCapacity = DefaultSendQueueCapacity
	}
	if o.MaxChannelsPerConnection == 0 {
		o.MaxChannelsPerConnection = DefaultMaxChannelsPerConnection
	}
	if o.MaxFrameBytes == 0 {
		o.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if o.PingInterval == 0 {
		o.PingInterval = DefaultPingInterval
	}
	if o.PongTimeout == 0 {
		o.PongTimeout = DefaultPongTimeout
	}
	if o.WriteTimeout == 0 {
		o.WriteTimeout = DefaultWriteTimeout
	}
	if o.ShutdownDrain == 0 {
		o.ShutdownDrain = DefaultShutdownDrain
	}
	if o.SubscriptionTTL == 0 {
		o.SubscriptionTTL = DefaultSubscriptionTTL
	}
	return o
}

// Validate reports whether o — already normalised — can produce a working
// gateway.
//
// It fails at construction rather than at the first connection, loudly, which is
// the rule the rest of this repository follows: a bad deployment should die
// before it serves traffic, not after it has served some.
func (o Options) Validate() error {
	switch {
	case o.Logger == nil:
		return fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	case o.SendQueueCapacity < 1:
		return fmt.Errorf("%w: SendQueueCapacity is %d; a queue of zero drops every frame "+
			"the instant it is enqueued", ErrInvalidOptions, o.SendQueueCapacity)
	case o.MaxChannelsPerConnection < 1:
		return fmt.Errorf("%w: MaxChannelsPerConnection is %d; a connection that may hold no "+
			"channels can never receive anything", ErrInvalidOptions, o.MaxChannelsPerConnection)
	case o.MaxFrameBytes < minUsefulFrameBytes:
		return fmt.Errorf("%w: MaxFrameBytes is %d; below %d even a single-channel subscribe "+
			"would be refused", ErrInvalidOptions, o.MaxFrameBytes, minUsefulFrameBytes)
	case o.PingInterval <= 0:
		return fmt.Errorf("%w: PingInterval is %s; without a heartbeat a half-open connection "+
			"is never reaped and holds its send queue for ever", ErrInvalidOptions, o.PingInterval)
	case o.PongTimeout <= 0:
		return fmt.Errorf("%w: PongTimeout is %s", ErrInvalidOptions, o.PongTimeout)
	case o.PongTimeout >= o.PingInterval:
		// Two pings in flight would make the timeout measure something other
		// than "this connection failed to answer one ping".
		return fmt.Errorf("%w: PongTimeout (%s) must be strictly less than PingInterval (%s), "+
			"so a reap decision is made from one outstanding ping",
			ErrInvalidOptions, o.PongTimeout, o.PingInterval)
	case o.WriteTimeout <= 0:
		return fmt.Errorf("%w: WriteTimeout is %s; an unbounded write parks a goroutine on a "+
			"peer that has stopped reading", ErrInvalidOptions, o.WriteTimeout)
	case o.ShutdownDrain <= 0:
		return fmt.Errorf("%w: ShutdownDrain is %s", ErrInvalidOptions, o.ShutdownDrain)
	case o.SubscriptionTTL <= 0:
		return fmt.Errorf("%w: SubscriptionTTL is %s", ErrInvalidOptions, o.SubscriptionTTL)
	}
	return nil
}

// minUsefulFrameBytes is the smallest frame ceiling that still admits a
// one-channel subscribe: `{"type":"subscribe","id":"...","channels":["..."]}`
// with a full-length identifier and a full-length request id. Below it the
// gateway would be configured to refuse every request, which is a mistake worth
// failing startup over rather than discovering from a client.
const minUsefulFrameBytes = 64 + MaxRequestIDLen + MaxChannelLen
