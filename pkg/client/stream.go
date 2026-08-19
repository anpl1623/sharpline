package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// The WebSocket half of the SDK: the client end of the snapshot-then-delta
// protocol CLAUDE.md §5 specifies.
//
// # Why this file re-declares the protocol instead of importing it
//
// The gateway's wire protocol lives in internal/wsgw. This package deliberately
// does not import it, for exactly the reason doc.go gives about the generated
// REST types: Go forbids importing an `internal/` package from outside its
// module, so an SDK that imported internal/wsgw would be an SDK nobody outside
// this repository could compile against. The frame kinds, the subprotocol names
// and the error codes are therefore re-declared here as the CLIENT's view of the
// contract.
//
// The duplication is bounded and it is checked rather than assumed:
// test/integration/wsgw_test.go drives this client against a real
// internal/wsgw gateway over a real broker, so a divergence between the two
// declarations fails a test rather than a customer.
//
// # There is no logger here either
//
// doc.go states the rule for the REST client and it is not weaker for a socket:
// "an SDK that logs requests is an SDK that eventually logs an Authorization
// header". A stream carries a bearer token through its handshake, so a logger on
// this type would be the most likely place in the whole package for one to
// escape.
//
// Instead every lifecycle transition this client makes is an EVENT the caller
// receives from [Stream.Next] — a connection opening, a connection dropping with
// the transport error that ended it, the backoff about to be waited, a sequence
// gap, a server-ordered resync. A caller that wants those in its log writes four
// lines and owns the redaction decision, which is the same trade [Doer] makes for
// request logging.
//
// # Sequence gaps are detected here, not only reported by the server
//
// CLAUDE.md §5: "Every message carries a monotonic sequence number; a gap
// triggers client resync." The server tells a client about the gaps it CAUSED
// (its own send queue overflowed and it discarded the buffer). It cannot tell a
// client about a gap it did not cause, and a client that only reacted to
// `resync` frames would be trusting the very component whose failure the
// sequence number exists to detect. So this client tracks the sequence itself
// and treats a jump as a gap, whether or not a resync frame follows.

// -----------------------------------------------------------------------------
// Protocol constants
// -----------------------------------------------------------------------------

const (
	// StreamProtocol is the subprotocol this client offers and requires the
	// server to select. It is versioned in the NAME rather than in a field,
	// because RFC 6455 already gives the handshake a negotiation slot.
	//
	// A server that selects anything else — including nothing — is refused
	// rather than spoken to: an unversioned connection is one whose frame
	// shapes can change under a client that has no way to notice.
	StreamProtocol = "sharpline.v1"

	// bearerSubprotocolPrefix carries the access token as a second subprotocol
	// offer, "sharpline.bearer.<jwt>".
	//
	// # Why the token goes here and never in the URL
	//
	// A browser's `new WebSocket(url, protocols)` can set exactly one request
	// header, Sec-WebSocket-Protocol. There is no options bag and no way to
	// attach an Authorization header, so the browser's only two choices are the
	// subprotocol list and the query string — and a query string is written to
	// every access log in the path, sent in Referer headers, kept in browser
	// history, and pasted into chat windows when somebody asks why their
	// connection is failing.
	//
	// This SDK is not a browser and could use `Authorization: Bearer`, which the
	// gateway also accepts. It uses the subprotocol anyway, so that the Go SDK
	// and the TypeScript client exercise the SAME mechanism: a credential path
	// that only one of two clients uses is a credential path that gets broken by
	// a change nobody tested against it.
	bearerSubprotocolPrefix = "sharpline.bearer."

	// DefaultStreamPath is the path the reverse proxy forwards to the gateway.
	// It is at the ROOT rather than under [DefaultBasePath]: the WebSocket
	// gateway is a separate service from the REST API and the proxy routes it by
	// path, so "/api/v1" is not part of its address.
	DefaultStreamPath = "/ws"
)

// Defaults for [StreamOptions]. Each is overridable; none is zero-valued into
// something dangerous.
const (
	// DefaultStreamBuffer is how many events [Stream.Next] may fall behind by
	// before the reader stops reading the socket.
	//
	// It deliberately matches the gateway's own default send-queue capacity. The
	// two queues are the two halves of one backpressure story (see
	// [StreamOptions.Buffer]), and sizing them alike means a client that pauses
	// for a garbage collection absorbs the same burst on both sides.
	DefaultStreamBuffer = 256

	// DefaultStreamReadLimit bounds one inbound frame.
	//
	// It is far above coder/websocket's own 32 KiB default, and it has to be: a
	// snapshot of a league channel carries every market on that league in one
	// frame, and 32 KiB is a few dozen markets. A read limit that is too small
	// does not degrade — it kills the connection on the first busy snapshot,
	// which then happens again on every reconnect.
	DefaultStreamReadLimit = 8 << 20

	// DefaultHandshakeTimeout bounds the upgrade, and only the upgrade. It is
	// emphatically not a deadline on the connection: a stream is long-lived by
	// definition and a deadline on the whole thing would sever it.
	DefaultHandshakeTimeout = 30 * time.Second

	// DefaultStreamWriteTimeout bounds one outbound frame. Client frames are
	// tiny requests, so this only ever fires against a peer that has stopped
	// reading.
	DefaultStreamWriteTimeout = 10 * time.Second

	// DefaultStreamBaseDelay and DefaultStreamMaxDelay bound the reconnect
	// backoff.
	//
	// The ceiling is 30 seconds rather than the REST client's 2, because the two
	// are waiting for different things: a retried GET is waiting out a hiccup,
	// while a reconnecting stream is frequently waiting out a deploy. Retrying a
	// rolling restart every two seconds is how a fleet of clients turns a
	// deployment into a thundering herd against the replica that came back
	// first.
	DefaultStreamBaseDelay = 500 * time.Millisecond
	DefaultStreamMaxDelay  = 30 * time.Second

	// maxStreamChannelLen bounds one channel string before it is sent. It
	// mirrors the gateway's own ceiling ("market" + ":" + a 128-byte
	// identifier), so an obviously impossible channel is refused here rather
	// than costing a round trip.
	maxStreamChannelLen = 135
)

// Stream sentinels. Match with errors.Is.
var (
	// ErrStreamClosed is returned by [Stream.Next] and the request methods once
	// the stream has been closed by the caller.
	ErrStreamClosed = errors.New("sharpline: stream closed")

	// ErrStreamProtocol means the server did not speak the protocol this client
	// requires: it selected a different subprotocol or none, sent a binary
	// frame, or sent something that is not a frame this build reads.
	ErrStreamProtocol = errors.New("sharpline: stream protocol error")

	// ErrTokenInURL means the caller supplied a stream URL carrying a
	// credential in its query string.
	//
	// It is refused HERE, before a connection is attempted, rather than left to
	// the gateway's identical refusal. The gateway can only tell a client after
	// the URL has already been written to whatever access log sits in front of
	// it; this SDK can refuse before it is sent at all, which is the only point
	// at which the mistake costs nothing.
	ErrTokenInURL = errors.New("sharpline: access token in the stream URL")

	// ErrStreamReconnectExhausted means the reconnect budget in
	// [StreamBackoff.MaxAttempts] ran out. The wrapped cause is the last
	// transport failure.
	ErrStreamReconnectExhausted = errors.New("sharpline: stream reconnect attempts exhausted")
)

// refusedStreamQueryParams are the query parameter names that cause
// [ErrTokenInURL]. It is the same closed list the gateway refuses, restated
// rather than imported for the reason at the top of this file — a heuristic
// ("does this value look like a JWT?") would refuse a legitimate parameter that
// happened to contain two dots and would miss a token under a name nobody
// thought of, so it would be both wrong and incomplete.
var refusedStreamQueryParams = []string{
	"token", "access_token", "accesstoken", "jwt", "bearer", "authorization",
}

// -----------------------------------------------------------------------------
// Events
// -----------------------------------------------------------------------------

// StreamEventKind classifies a [StreamEvent]. It is a closed set: seven kinds mirror the
// server's frames and two are this client's own observations.
type StreamEventKind string

// The event kinds.
const (
	// StreamEventHello is the first event on every connection, including every
	// reconnection. [StreamEvent.ConnectionID] changes with it, and the sequence
	// space restarts — so a caller keeping per-connection state resets it here.
	StreamEventHello StreamEventKind = "hello"

	// StreamEventAck answers a subscribe or unsubscribe. [StreamEvent.Subscribed] and
	// [StreamEvent.Rejected] are both populated: a partial success is reported as one.
	StreamEventAck StreamEventKind = "ack"

	// StreamEventSnapshot is a channel's current markets. An EMPTY [StreamEvent.Markets] is
	// a correct snapshot of a channel that holds nothing — a league with no
	// scheduled events — and is not an error.
	StreamEventSnapshot StreamEventKind = "snapshot"

	// StreamEventDelta is one market's change. Either [StreamEvent.Market] carries the new
	// document or [StreamEvent.Removed] names a market that no longer exists; exactly
	// one of them is set, and [StreamEvent.IsRemoval] is the predicate.
	//
	// A removal MUST be handled. On a compacted topic a tombstone means the
	// market is gone for ever, so a client that ignores one leaves it on the
	// board permanently — no further record for that key is coming.
	StreamEventDelta StreamEventKind = "delta"

	// StreamEventResync is the SERVER telling this client its stream has a hole,
	// because the server discarded this connection's pending buffer. See
	// [StreamEventGap] for the difference.
	StreamEventResync StreamEventKind = "resync"

	// StreamEventError is a coded failure from the server. [StreamEvent.Code] is what to
	// branch on; [StreamEvent.Message] is for a human reading a console.
	StreamEventError StreamEventKind = "error"

	// StreamEventPong answers a [Stream.Ping]. It is the APPLICATION-level pong, not
	// the RFC 6455 control frame the transport exchanges underneath — a client
	// behind a proxy that answers control frames on its behalf would look alive
	// while receiving nothing, and this round trip is what distinguishes them.
	StreamEventPong StreamEventKind = "pong"

	// StreamEventGap is THIS CLIENT observing a jump in the sequence numbers.
	//
	// It is distinct from [StreamEventResync] on purpose. A resync frame is the server
	// reporting a hole it created and knows the size of; a gap is this client
	// detecting a hole from the numbers alone, which is the only observation
	// available when the loss happened somewhere the server cannot see. Both
	// lead to the same recovery, and telling them apart is the difference
	// between "the gateway shed load" and "something between us dropped
	// frames".
	StreamEventGap StreamEventKind = "gap"

	// StreamEventDisconnected reports that the connection ended. [StreamEvent.Err] carries
	// the transport failure, and [StreamEvent.Attempt] and [StreamEvent.RetryIn] describe
	// the reconnection about to be attempted. A stream with reconnection
	// disabled emits this once and then ends.
	StreamEventDisconnected StreamEventKind = "disconnected"
)

// String implements fmt.Stringer.
func (k StreamEventKind) String() string { return string(k) }

// StreamErrorCode is the server's bounded classification on an [StreamEventError].
// It is what a client branches on; the accompanying message is never parsed.
type StreamErrorCode string

// The error codes the gateway emits.
const (
	StreamCodeMalformedFrame StreamErrorCode = "malformed_frame"
	StreamCodeUnknownType    StreamErrorCode = "unknown_type"
	StreamCodeFrameTooLarge  StreamErrorCode = "frame_too_large"
	StreamCodeInvalidChannel StreamErrorCode = "invalid_channel"
	StreamCodeChannelLimit   StreamErrorCode = "channel_limit"
	StreamCodeUnauthorized   StreamErrorCode = "unauthorized"
	StreamCodeGoingAway      StreamErrorCode = "going_away"
	StreamCodeInternal       StreamErrorCode = "internal"
)

// RejectedStreamChannel is one channel a subscribe request was refused for,
// with the server's bounded reason.
type RejectedStreamChannel struct {
	// Channel is the client's own string, echoed back bounded so a caller that
	// sent forty channels can tell which one was refused.
	Channel string `json:"channel"`
	// Reason is a value from the server's closed set: malformed, unknown_kind,
	// invalid_id, too_long, limit_reached, duplicate.
	Reason string `json:"reason"`
}

// StreamEvent is one thing that happened on a [Stream].
//
// It is ONE struct with a [StreamEventKind] rather than a sealed interface, and that
// mirrors the wire protocol deliberately: the server's delta frame already has
// two shapes in one kind, and a caller's handler for both is "replace what I
// hold for this market id". A hierarchy of nine types would make the common
// switch longer without making any case clearer.
//
// Fields not named by the kind are zero. The kind-specific fields are documented
// on [StreamEventKind].
type StreamEvent struct {
	// Kind says which event this is and therefore which fields are populated.
	Kind StreamEventKind

	// Seq is the server's per-connection sequence number for the frame that
	// produced this event. It restarts at 1 on every connection, which is why
	// [StreamEvent.ConnectionID] exists. Zero on [StreamEventGap] and [StreamEventDisconnected],
	// which are this client's own observations and carry no frame.
	Seq uint64

	// TS is when the server built the frame. It is a DIAGNOSTIC and is never a
	// staleness subtrahend: the observation instant a freshness measurement is
	// taken against lives on the price itself, inside the market document.
	TS time.Time

	// ID echoes the request id this client sent, when the event answers a
	// request. Empty otherwise.
	ID string

	// ConnectionID identifies the connection this event arrived on — and, on
	// [StreamEventDisconnected], the connection that just ended. It is set on
	// every event, including the two local ones, so a caller can attribute an
	// event without tracking the hello itself.
	ConnectionID string

	// Channel is the channel a snapshot or a delta was delivered on. The same
	// market is delivered on up to three channels, so this is what lets a client
	// holding two of them attribute the frame.
	Channel string

	// Markets is a snapshot's market documents, byte for byte as the pricer
	// published them. Empty is a correct snapshot.
	Markets []json.RawMessage

	// Market is a delta's new document, byte for byte. Nil on a removal.
	Market json.RawMessage

	// Removed is the market id a removal delta deletes. Empty on an update.
	Removed string

	// Subscribed and Rejected are an ack's two halves.
	Subscribed []string
	Rejected   []RejectedStreamChannel

	// Hello carries the opening frame's fields. Set on [StreamEventHello] only.
	Hello *StreamHello

	// Gap describes a sequence hole, on [StreamEventGap] and on [StreamEventResync] alike.
	// On a resync it is the SERVER's account of the hole; on a gap it is this
	// client's.
	Gap *StreamGap

	// Reason is the server's bounded resync reason on [StreamEventResync]:
	// slow_consumer, client_requested or presence_lost.
	Reason string

	// Code and Message are an [StreamEventError]'s payload.
	Code    StreamErrorCode
	Message string

	// Err is the transport failure that ended a connection, on
	// [StreamEventDisconnected]. It never contains a credential.
	Err error

	// Attempt and RetryIn describe the reconnection about to be attempted, on
	// [StreamEventDisconnected]. Attempt counts CONSECUTIVE failures, so it is 1 for
	// the first reconnect after a healthy connection and resets once a
	// connection is established. RetryIn is zero when no reconnection will be
	// attempted.
	Attempt int
	RetryIn time.Duration
}

// IsRemoval reports whether a delta deletes a market rather than updating one.
func (e StreamEvent) IsRemoval() bool { return e.Kind == StreamEventDelta && e.Removed != "" }

// StreamHello is the opening frame's payload.
type StreamHello struct {
	// ConnectionID identifies this connection. The sequence space is scoped to
	// it, so a reconnect is not an epoch problem: the client resets its
	// expectation whenever this changes.
	ConnectionID string
	// Protocol is the negotiated subprotocol, echoed by the server so a client
	// can assert it got what it offered rather than trusting a handshake it did
	// not inspect.
	Protocol string
	// Heartbeat is the server's ping period. A client sizing its own liveness
	// timer should take it from here rather than hard-coding a second copy.
	Heartbeat time.Duration
	// SessionID is the durable session key this connection is bound to.
	SessionID string
	// Resumed reports that the server restored a subscription set from its
	// durable store — which is what makes affinity-free load balancing
	// observable: reconnect, land on another replica, and the channels come
	// back.
	Resumed bool
	// Authenticated reports that a credential was presented AND verified. False
	// is the normal case: market data is public.
	Authenticated bool
	// Channels are the restored channels.
	Channels []string
}

// StreamGap is a hole in the sequence stream.
type StreamGap struct {
	// From and To bracket the sequence numbers that were not received,
	// inclusive.
	From uint64
	To   uint64
	// Dropped is how many frames the hole covers.
	Dropped int
	// ServerReported distinguishes the two observations: true when the server
	// sent a resync frame naming the hole, false when this client inferred it
	// from the numbers alone.
	ServerReported bool
}

// String renders the gap for a log line.
func (g StreamGap) String() string {
	origin := "detected locally"
	if g.ServerReported {
		origin = "reported by the server"
	}
	return fmt.Sprintf("%d frames missing, seq %d-%d (%s)", g.Dropped, g.From, g.To, origin)
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// StreamBackoff bounds reconnection.
//
// It is a distinct type from [RetryPolicy] and shares its arithmetic on purpose.
// The DELAY is the same full-jitter schedule and the argument for it is written
// once, at [RetryPolicy.delay]; the ATTEMPT semantics are different enough that
// one struct would be a trap. A RetryPolicy's MaxAttempts of zero means "three",
// because an HTTP call must eventually give up and return to its caller. A
// stream's zero means "never give up", because a stream that stopped
// reconnecting after three deploys would be a stream nobody could leave running.
type StreamBackoff struct {
	// BaseDelay is the first backoff window. Zero means
	// [DefaultStreamBaseDelay].
	BaseDelay time.Duration
	// MaxDelay caps the window. Zero means [DefaultStreamMaxDelay].
	MaxDelay time.Duration
	// MaxAttempts bounds CONSECUTIVE reconnection attempts. Zero means
	// unbounded; a successful connection resets the count. 1 means "reconnect
	// once, then give up"; use [StreamOptions.NoReconnect] to disable
	// reconnection entirely rather than setting a negative value here.
	MaxAttempts int
	// Jitter returns a value in [0,1). Zero means math/rand/v2. It exists so a
	// test can make the backoff deterministic without sleeping for real.
	Jitter func() float64
}

// delay returns the wait before attempt n (1-based), reusing [RetryPolicy]'s
// full-jitter schedule rather than restating it.
func (b StreamBackoff) delay(attempt int) time.Duration {
	base := b.BaseDelay
	if base <= 0 {
		base = DefaultStreamBaseDelay
	}
	maxDelay := b.MaxDelay
	if maxDelay <= 0 {
		maxDelay = DefaultStreamMaxDelay
	}
	return RetryPolicy{BaseDelay: base, MaxDelay: maxDelay, Jitter: b.Jitter}.delay(attempt)
}

// maxDelay is the ceiling the schedule saturates at. It doubles as the
// threshold [Stream.run] uses to decide that a connection was healthy.
func (b StreamBackoff) maxDelay() time.Duration {
	if b.MaxDelay <= 0 {
		return DefaultStreamMaxDelay
	}
	return b.MaxDelay
}

// exhausted reports whether attempt n exceeds the budget.
func (b StreamBackoff) exhausted(attempt int) bool {
	return b.MaxAttempts > 0 && attempt > b.MaxAttempts
}

// StreamSocket is the transport a [Stream] reads and writes.
//
// It is declared by the CONSUMER (CLAUDE.md §12) and it deliberately mentions no
// type from the WebSocket library. That is not purity: this package is the
// PUBLIC SDK, and an exported interface carrying `websocket.MessageType` would
// make every caller who wants to substitute a transport take a dependency on
// coder/websocket at a version this module pins.
//
// One goroutine calls Receive and another may call Send concurrently;
// implementations must tolerate that. Nothing calls Send concurrently with
// itself — [Stream] serialises its own writes.
type StreamSocket interface {
	// Subprotocol returns the subprotocol the server selected. [Stream] refuses
	// any connection whose value is not [StreamProtocol].
	Subprotocol() string
	// Receive returns the next text frame's payload, or an error once the
	// connection has ended.
	Receive(ctx context.Context) ([]byte, error)
	// Send writes one text frame.
	Send(ctx context.Context, payload []byte) error
	// Close ends the connection with a normal-closure status and the given
	// reason.
	Close(reason string) error
}

// StreamDialer opens a [StreamSocket].
//
// The default implementation is coder/websocket. Substituting one is how a
// caller adds a custom TLS configuration this package does not model, routes
// through a tunnel, or drives the client from a test.
type StreamDialer interface {
	// DialStream opens a connection to rawURL, offering subprotocols in the
	// order given. It must return an error rather than a socket when the
	// handshake fails, and that error must not contain any element of
	// subprotocols — the bearer offer is a credential.
	DialStream(ctx context.Context, rawURL string, subprotocols []string) (StreamSocket, error)
}

// StreamOptions configures [Client.Stream].
//
// The zero value is usable: it streams from the client's own base URL, anonymous
// unless the client carries a session, with the default buffer and backoff.
type StreamOptions struct {
	// URL is the gateway's address. Empty derives it from the client's base URL
	// — scheme swapped to ws/wss, host kept, path [DefaultStreamPath] — which is
	// the topology the reverse proxy serves.
	//
	// http and https are accepted and converted. A URL carrying a credential in
	// its query string is refused with [ErrTokenInURL].
	URL string

	// Token is the access token to present. Empty falls back to the client's
	// [TokenSource] when it has one, which is what makes
	// `c.WithSession(sess).Stream(ctx, …)` work.
	//
	// It is presented as a subprotocol offer and NEVER as a query parameter. It
	// is not stored on the [Stream] when it came from a TokenSource: a
	// reconnection asks the source again, so a stream that outlives its access
	// token reconnects with a fresh one instead of a rejected one.
	Token string

	// Channels are subscribed as soon as the connection opens, and again after
	// every reconnection. They are the DESIRED set: [Stream.Subscribe] and
	// [Stream.Unsubscribe] amend it, and it is what a reconnect restores.
	Channels []string

	// Dialer opens the socket. Nil means coder/websocket.
	Dialer StreamDialer

	// HTTPClient performs the handshake when Dialer is nil. Nil means
	// http.DefaultClient. It is the supported way to add TLS settings or a
	// proxy without replacing the whole dialer.
	//
	// Its Timeout, if set, bounds the HANDSHAKE only — coder/websocket applies
	// it to the upgrade and then clears it, so it cannot sever the stream.
	HTTPClient *http.Client

	// Buffer is how many events may queue between the socket and
	// [Stream.Next]. Zero means [DefaultStreamBuffer].
	//
	// # A full buffer blocks, and that is the design
	//
	// This client does NOT drop events when a caller falls behind. It stops
	// reading the socket, which makes it the slow consumer the gateway is built
	// to shed: the server discards this connection's pending buffer, counts a
	// drop, and sends a resync, which this client then services. So the failure
	// mode of a slow caller is a resync — visible on both sides, in a metric and
	// in an event — rather than a silently incomplete board.
	Buffer int

	// ReadLimit bounds one inbound frame. Zero means
	// [DefaultStreamReadLimit]. See that constant on why the library's default
	// is far too small for a league snapshot.
	ReadLimit int64

	// HandshakeTimeout bounds the upgrade. Zero means
	// [DefaultHandshakeTimeout]. It is not a deadline on the connection.
	HandshakeTimeout time.Duration

	// WriteTimeout bounds one outbound frame. Zero means
	// [DefaultStreamWriteTimeout].
	WriteTimeout time.Duration

	// Backoff bounds reconnection.
	Backoff StreamBackoff

	// NoReconnect makes the stream end at the first disconnection rather than
	// reconnecting.
	//
	// Negatively named so the zero value reconnects, which is the behaviour
	// CLAUDE.md §7 requires of the browser client and the only sane default for
	// a long-lived stream. It exists for a one-shot tool that wants a snapshot
	// and an exit code.
	NoReconnect bool
}

// -----------------------------------------------------------------------------
// Stream
// -----------------------------------------------------------------------------

// Stream is a live subscription to the odds gateway.
//
// It owns one goroutine, which connects, reads, and reconnects. Everything a
// caller observes arrives through [Stream.Next]; everything a caller asks for
// goes through [Stream.Subscribe], [Stream.Unsubscribe], [Stream.Resync] and
// [Stream.Ping]. It is safe for concurrent use, though [Stream.Next] is
// single-consumer by nature — two goroutines calling it split the event stream
// between them, which is almost never what anyone wants.
type Stream struct {
	url       string
	tokens    TokenSource
	staticTok string
	dial      StreamDialer

	buffer     int
	writeWait  time.Duration
	backoff    StreamBackoff
	reconnect  bool
	handshake  time.Duration
	readLimit  int64
	httpClient *http.Client

	events chan StreamEvent
	done   chan struct{}

	cancel context.CancelFunc

	closeOnce sync.Once

	// mu guards everything a caller's goroutine and the run goroutine both
	// touch: the live socket, the desired channel set, the request counter and
	// the terminal error.
	mu       sync.Mutex
	sock     StreamSocket
	connID   string
	desired  []string
	requests uint64
	err      error
}

// Stream opens a WebSocket subscription.
//
// It returns once the FIRST connection has been established and its `hello`
// frame read, so a caller that gets a nil error has a live connection and a
// server that agreed on the protocol. Everything after that — including every
// reconnection — is reported through [Stream.Next] rather than through a
// returned error, because a stream that reconnects cannot report a transient
// failure to a call that has already returned.
//
// The returned Stream must be closed.
func (c *Client) Stream(ctx context.Context, opts StreamOptions) (*Stream, error) {
	if ctx == nil {
		return nil, fmt.Errorf("sharpline: stream: nil context")
	}

	target, err := c.streamURL(opts.URL)
	if err != nil {
		return nil, err
	}

	channels := make([]string, 0, len(opts.Channels))
	for _, ch := range opts.Channels {
		if err := validStreamChannel(ch); err != nil {
			return nil, err
		}
		channels = append(channels, ch)
	}

	dialer := opts.Dialer
	if dialer == nil {
		dialer = &websocketDialer{
			client:    opts.HTTPClient,
			readLimit: positiveInt64Or(opts.ReadLimit, DefaultStreamReadLimit),
		}
	}

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	s := &Stream{
		url:        target,
		staticTok:  opts.Token,
		dial:       dialer,
		buffer:     positiveIntOr(opts.Buffer, DefaultStreamBuffer),
		writeWait:  positiveDurationOr(opts.WriteTimeout, DefaultStreamWriteTimeout),
		backoff:    opts.Backoff,
		reconnect:  !opts.NoReconnect,
		handshake:  positiveDurationOr(opts.HandshakeTimeout, DefaultHandshakeTimeout),
		readLimit:  positiveInt64Or(opts.ReadLimit, DefaultStreamReadLimit),
		httpClient: opts.HTTPClient,
		desired:    channels,
		cancel:     cancel,
	}
	// Only when no explicit token was given: an explicit one is the caller's
	// deliberate override and must not be silently replaced on a reconnect by a
	// different credential.
	if opts.Token == "" {
		s.tokens = c.tokens
	}
	s.events = make(chan StreamEvent, s.buffer)
	s.done = make(chan struct{})

	// The FIRST connection is synchronous, so a caller holding a nil error holds
	// a working stream. Its context is the caller's, so a caller's deadline
	// bounds the dial; the run goroutine's context is detached from it, because
	// a stream must outlive the call that opened it.
	sock, hello, err := s.connect(ctx)
	if err != nil {
		cancel()
		return nil, err
	}

	go s.run(runCtx, sock, hello)
	return s, nil
}

// streamURL resolves the gateway address, refusing one that carries a
// credential.
func (c *Client) streamURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		u := *c.base
		switch u.Scheme {
		case "http":
			u.Scheme = "ws"
		case "https":
			u.Scheme = "wss"
		}
		u.Path = DefaultStreamPath
		u.RawQuery = ""
		u.Fragment = ""
		return u.String(), nil
	}

	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: stream URL: %w", ErrInvalidOptions, err)
	}
	switch u.Scheme {
	case "ws", "wss":
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("%w: stream URL scheme is %q, want ws, wss, http or https",
			ErrInvalidOptions, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%w: stream URL has no host", ErrInvalidOptions)
	}

	q := u.Query()
	for _, name := range refusedStreamQueryParams {
		if q.Has(name) {
			// The VALUE is not in the error, because the error is the thing that
			// reaches a log line and the value is the credential.
			return "", fmt.Errorf("%w: %q must not appear in the stream URL; the token is "+
				"presented as a %q subprotocol offer instead",
				ErrTokenInURL, name, bearerSubprotocolPrefix+"<token>")
		}
	}
	if u.Path == "" {
		u.Path = DefaultStreamPath
	}
	return u.String(), nil
}

// validStreamChannel refuses a channel this client can see is impossible.
//
// It is deliberately shallow. The gateway owns the channel grammar and answers
// with a bounded [RejectedStreamChannel.Reason] from a closed set; duplicating
// that parse here would create a second grammar to keep in step, and the SDK
// would eventually refuse a channel a newer gateway accepts. What is checked
// here is only what costs a round trip to discover: empty, and longer than any
// legal channel.
func validStreamChannel(ch string) error {
	switch {
	case strings.TrimSpace(ch) == "":
		return fmt.Errorf("%w: empty channel", ErrInvalidOptions)
	case len(ch) > maxStreamChannelLen:
		return fmt.Errorf("%w: channel is %d bytes, no legal channel exceeds %d",
			ErrInvalidOptions, len(ch), maxStreamChannelLen)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Caller-facing API
// -----------------------------------------------------------------------------

// Next returns the next event, blocking until one arrives.
//
// # Why this is a method and not an exported channel
//
// A channel would have to be closed to signal the end of the stream, and the
// reason it ended would then live somewhere else — an Err() method the caller
// must remember to consult after the range loop drains. That is two objects to
// keep in step, and the failure mode of forgetting the second one is a program
// that treats a permanent authentication failure as a clean shutdown.
//
// Next returns the reason in the same call that reports the end. It also takes
// its own context, so a caller can poll with a deadline of its own without the
// stream having to know about it.
//
// The error is [ErrStreamClosed] after [Stream.Close], ctx.Err() if the caller's
// context ends first, and otherwise the failure that ended the stream.
func (s *Stream) Next(ctx context.Context) (StreamEvent, error) {
	if ctx == nil {
		return StreamEvent{}, fmt.Errorf("sharpline: stream: nil context")
	}
	select {
	case ev, ok := <-s.events:
		if ok {
			return ev, nil
		}
	case <-ctx.Done():
		return StreamEvent{}, ctx.Err()
	case <-s.done:
	}

	// Drain before reporting the end: the run goroutine may have queued events
	// and then failed, and discarding them would lose the last snapshot a caller
	// received in favour of the error that followed it.
	select {
	case ev, ok := <-s.events:
		if ok {
			return ev, nil
		}
	default:
	}
	return StreamEvent{}, s.Err()
}

// Err returns the failure that ended the stream, or nil while it is running.
// [ErrStreamClosed] means the caller closed it.
func (s *Stream) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// ConnectionID returns the id of the connection currently serving this stream.
// It changes on every reconnection, which is what makes the per-connection
// sequence space unambiguous.
func (s *Stream) ConnectionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connID
}

// Channels returns the desired subscription set: what this stream will hold once
// its requests are acknowledged, and what a reconnection restores.
func (s *Stream) Channels() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.desired...)
}

// Subscribe adds channels to the subscription.
//
// It records the DESIRE first and then sends the request, so a reconnection
// restores what the caller asked for even if this request never reached the
// server. It returns when the frame has been written, NOT when the server has
// acknowledged it: a subscribe is answered by an ack and then by one snapshot
// per accepted channel, so a call that returned on the ack would return before
// the data the caller actually wanted had arrived. The ack — including any
// bounded rejections — is delivered as an [StreamEventAck].
//
// While the stream is between connections the request is recorded and nothing is
// sent; it is applied when the connection comes back.
func (s *Stream) Subscribe(ctx context.Context, channels ...string) error {
	return s.amend(ctx, clientSubscribe, channels, true)
}

// Unsubscribe removes channels from the subscription. A channel the connection
// does not hold is a no-op on the server, not an error.
func (s *Stream) Unsubscribe(ctx context.Context, channels ...string) error {
	return s.amend(ctx, clientUnsubscribe, channels, false)
}

// Resync asks the server for a fresh snapshot of the named channels. An EMPTY
// list means every channel this connection holds, which is what this client
// sends on detecting a gap.
//
// It is exported because a caller with out-of-band knowledge that its state is
// wrong — a UI resuming from a background tab, a process that paused — can ask
// for one without waiting to observe a gap.
func (s *Stream) Resync(ctx context.Context, channels ...string) error {
	for _, ch := range channels {
		if err := validStreamChannel(ch); err != nil {
			return err
		}
	}
	return s.send(ctx, clientFrame{Type: clientResync, ID: s.nextRequestID(), Channels: channels})
}

// Ping asks the server for an application-level pong, which arrives as an
// [StreamEventPong] carrying the same request id.
func (s *Stream) Ping(ctx context.Context) error {
	return s.send(ctx, clientFrame{Type: clientPing, ID: s.nextRequestID()})
}

// Close ends the stream and releases its goroutine.
//
// It is idempotent. After it returns, [Stream.Next] reports [ErrStreamClosed].
//
// # The cancel comes BEFORE the socket close, and the order is load-bearing
//
// [Stream.run] decides whether a connection that ended was a failure to
// reconnect from or the caller shutting down, and it decides it by reading
// ctx.Err() the moment [Stream.serve] returns. The only thing that makes that
// read correct is that the cancellation is already visible by the time the
// socket can report the error this function caused.
//
// Closing the socket first inverts that. The read loop wakes on a closed
// socket, finds ctx.Err() still nil, concludes the connection dropped on its
// own, and emits a StreamEventDisconnected carrying a retry delay for a stream
// the caller has just shut down. The event lands in the buffer, [Stream.Next]
// hands it to the caller, and the ErrStreamClosed this comment promises never
// arrives — the caller sees a reconnection notice as the last word on a stream
// it closed itself. The window is small, real, and was reproducible under
// `go test -race`.
//
// The socket is still closed explicitly rather than being left to the
// cancellation, because Close is what sends the WebSocket close frame that
// tells the gateway this was a clean departure rather than a dead peer to reap.
func (s *Stream) Close() error {
	var closeErr error
	s.closeOnce.Do(func() {
		s.mu.Lock()
		if s.err == nil {
			s.err = ErrStreamClosed
		}
		sock := s.sock
		s.sock = nil
		s.mu.Unlock()

		s.cancel()

		if sock != nil {
			closeErr = sock.Close("client closing")
		}
	})
	<-s.done
	return closeErr
}

// amend records a subscription change and sends it.
func (s *Stream) amend(ctx context.Context, kind clientKind, channels []string, add bool) error {
	if len(channels) == 0 {
		return fmt.Errorf("%w: %s requires at least one channel", ErrInvalidOptions, kind)
	}
	for _, ch := range channels {
		if err := validStreamChannel(ch); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	if add {
		for _, ch := range channels {
			if !containsString(s.desired, ch) {
				s.desired = append(s.desired, ch)
			}
		}
	} else {
		s.desired = removeStrings(s.desired, channels)
	}
	s.mu.Unlock()

	return s.send(ctx, clientFrame{Type: kind, ID: s.nextRequestID(), Channels: channels})
}

// send writes one client frame on the live socket.
//
// A stream with no live socket is NOT an error: the desired set has already been
// amended and the reconnection will apply it. Returning a failure here would
// make every caller write a retry loop around a condition this type already
// recovers from.
func (s *Stream) send(ctx context.Context, f clientFrame) error {
	if ctx == nil {
		return fmt.Errorf("sharpline: stream: nil context")
	}

	s.mu.Lock()
	if s.err != nil {
		err := s.err
		s.mu.Unlock()
		return err
	}
	sock := s.sock
	s.mu.Unlock()

	if sock == nil {
		return nil
	}

	payload, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("sharpline: stream: encode %s: %w", f.Type, err)
	}

	writeCtx, cancel := context.WithTimeout(ctx, s.writeWait)
	defer cancel()
	if err := sock.Send(writeCtx, payload); err != nil {
		return fmt.Errorf("sharpline: stream: send %s: %w", f.Type, err)
	}
	return nil
}

// nextRequestID mints the correlation id echoed on the answering frame.
//
// A counter rather than a random value: the gateway bounds the id to 64
// printable ASCII bytes and echoes it, a counter is trivially within that, and a
// failing test names "r7" rather than a hex string nobody can correlate by eye.
// It is scoped to one Stream, so ids from two streams colliding is meaningless —
// nothing joins across connections.
func (s *Stream) nextRequestID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	return "r" + strconv.FormatUint(s.requests, 10)
}

// -----------------------------------------------------------------------------
// The connection loop
// -----------------------------------------------------------------------------

// run owns the connection for the life of the stream.
//
// It is handed the first socket already connected, because [Client.Stream]
// establishes it synchronously so that a caller holding a nil error holds a
// live stream.
//
// # When the reconnect counter resets
//
// It resets when a connection was served for longer than
// [StreamBackoff.MaxDelay], and not merely when one was established. The
// distinction matters against a server that accepts a handshake and closes
// immediately — a replica whose readiness probe is failing, an ingress with no
// backend — where resetting on connect would produce a reconnect every base
// delay for as long as the fault lasts, which is precisely the herd the backoff
// exists to prevent. A connection that outlived the longest wait the policy will
// ever impose is evidence that the server can serve this client; a shorter one
// is not.
func (s *Stream) run(ctx context.Context, sock StreamSocket, hello StreamEvent) {
	defer close(s.events)
	defer close(s.done)

	var (
		attempt int
		last    error
		// lastConn is the id of the connection that most recently ended, so a
		// disconnection event names the connection it is about rather than the
		// empty string the live one has already been cleared to.
		lastConn string
	)

	for {
		if sock != nil {
			s.adopt(sock, hello.ConnectionID)
			if !s.emit(ctx, hello) {
				s.finish(ErrStreamClosed)
				return
			}

			opened := time.Now()
			// The hello was read by connect, so serve starts EXPECTING the frame
			// after it. Starting from "nothing seen yet" would make a hole
			// between the hello and the next frame invisible, which is the one
			// hole a client cannot recover from by any other means: it has no
			// snapshot yet.
			last = s.serve(ctx, sock, hello.Seq+1)
			served := time.Since(opened)

			s.adopt(nil, "")
			_ = sock.Close("client reconnecting")
			lastConn = hello.ConnectionID
			sock, hello = nil, StreamEvent{}

			if ctx.Err() != nil {
				s.finish(ErrStreamClosed)
				return
			}
			if errors.Is(last, ErrStreamProtocol) {
				// A server that is not speaking this protocol will not start on
				// the next connection, and reconnecting into it would turn a
				// diagnosable failure into an unbounded loop of them.
				s.emit(ctx, StreamEvent{
					Kind: StreamEventDisconnected, ConnectionID: lastConn, Err: last,
				})
				s.finish(last)
				return
			}
			if served > s.backoff.maxDelay() {
				attempt = 0
			}
		}

		attempt++
		if !s.reconnect {
			s.emit(ctx, StreamEvent{
				Kind: StreamEventDisconnected, ConnectionID: lastConn, Err: last, Attempt: attempt,
			})
			s.finish(last)
			return
		}
		if s.backoff.exhausted(attempt) {
			s.emit(ctx, StreamEvent{
				Kind: StreamEventDisconnected, ConnectionID: lastConn, Err: last, Attempt: attempt,
			})
			s.finish(fmt.Errorf("%w after %d: %w", ErrStreamReconnectExhausted, attempt-1, last))
			return
		}

		wait := s.backoff.delay(attempt)
		if !s.emit(ctx, StreamEvent{
			Kind: StreamEventDisconnected, ConnectionID: lastConn,
			Err: last, Attempt: attempt, RetryIn: wait,
		}) {
			s.finish(ErrStreamClosed)
			return
		}
		if !sleepCtx(ctx, wait) {
			s.finish(ErrStreamClosed)
			return
		}

		next, nextHello, err := s.connect(ctx)
		if err != nil {
			if ctx.Err() != nil {
				s.finish(ErrStreamClosed)
				return
			}
			// A failed dial is another consecutive failure and re-enters the
			// same branch with sock still nil, so the backoff climbs exactly as
			// it does for a connection that dropped.
			last = err
			continue
		}
		sock, hello = next, nextHello
	}
}

// serve reads one connection until it ends, tracking the sequence and answering
// what needs answering.
//
// It returns the failure that ended the connection, never nil: a WebSocket that
// stops delivering has always stopped for a reason, and a nil here would be a
// clean stop this loop has no way to distinguish from a silent one.
func (s *Stream) serve(ctx context.Context, sock StreamSocket, expect uint64) error {
	var resyncPending bool

	for {
		payload, err := sock.Receive(ctx)
		if err != nil {
			return err
		}

		frame, err := decodeServerFrame(payload)
		if err != nil {
			return err
		}

		// The sequence check runs BEFORE the frame is interpreted, so a hole is
		// observed even when the frame that revealed it is itself a resync.
		if expect != 0 && frame.Seq != expect {
			if frame.Seq < expect {
				return fmt.Errorf("%w: sequence went backwards, %d after %d",
					ErrStreamProtocol, frame.Seq, expect-1)
			}
			gap := &StreamGap{From: expect, To: frame.Seq - 1, Dropped: int(frame.Seq - expect)}
			if !s.emit(ctx, StreamEvent{
				Kind: StreamEventGap, ConnectionID: s.ConnectionID(), TS: frame.TS, Gap: gap,
			}) {
				return ErrStreamClosed
			}
			if !resyncPending {
				resyncPending = true
				// An EMPTY channel list means every channel this connection
				// holds, which is the only thing a client can ask for: the
				// sequence number says a frame was lost and says nothing about
				// which channel it was for.
				//
				// This is a RESYNC REQUEST rather than a reconnection, and the
				// difference is deliberate. A gap means the server discarded
				// this connection's buffer, not that the socket is broken —
				// tearing down a healthy TCP connection would cost a handshake,
				// would lose the connection's durable session binding, and would
				// re-enter the reconnect backoff for a fault that has already
				// passed. The gateway's resync path re-snapshots every held
				// channel under the same lock that serialises delta publication,
				// so what comes back cannot be overtaken by an update.
				if err := s.send(ctx, clientFrame{
					Type: clientResync, ID: s.nextRequestID(),
				}); err != nil {
					return err
				}
			}
		}
		expect = frame.Seq + 1

		ev, err := s.eventFor(frame)
		if err != nil {
			return err
		}

		switch frame.Type {
		case kindHello:
			// A hello mid-connection is the server restarting the sequence
			// space under a client that has no way to know it happened.
			return fmt.Errorf("%w: a second hello arrived on one connection", ErrStreamProtocol)
		case kindSnapshot:
			resyncPending = false
		case kindResync:
			if !resyncPending {
				resyncPending = true
				if err := s.send(ctx, clientFrame{
					Type: clientResync, ID: s.nextRequestID(),
				}); err != nil {
					return err
				}
			}
		}

		if !s.emit(ctx, ev) {
			return ErrStreamClosed
		}
	}
}

// connect dials, reads the hello frame, and applies the desired subscription
// set.
//
// The hello is read HERE rather than in the serve loop because it is what proves
// the connection is usable: it names the connection id the sequence space is
// scoped to, and it reports which channels the server restored from its durable
// store — which is what the subscribe below must not ask for again.
func (s *Stream) connect(ctx context.Context) (StreamSocket, StreamEvent, error) {
	token, err := s.credential(ctx)
	if err != nil {
		return nil, StreamEvent{}, err
	}

	offers := []string{StreamProtocol}
	if token != "" {
		offers = append(offers, bearerSubprotocolPrefix+token)
	}

	dialCtx, cancel := context.WithTimeout(ctx, s.handshake)
	sock, err := s.dial.DialStream(dialCtx, s.url, offers)
	cancel()
	if err != nil {
		return nil, StreamEvent{}, fmt.Errorf("sharpline: stream: dial: %w", err)
	}

	if got := sock.Subprotocol(); got != StreamProtocol {
		_ = sock.Close("protocol not negotiated")
		return nil, StreamEvent{}, fmt.Errorf("%w: server selected subprotocol %q, want %q",
			ErrStreamProtocol, got, StreamProtocol)
	}

	// The hello read is bounded by the handshake budget too. A peer that
	// completes the upgrade and then sends nothing is a peer that has accepted a
	// connection it cannot serve, and waiting for it on the caller's context
	// would park this stream indefinitely on a socket that will never speak.
	helloCtx, helloCancel := context.WithTimeout(ctx, s.handshake)
	payload, err := sock.Receive(helloCtx)
	helloCancel()
	if err != nil {
		_ = sock.Close("no hello")
		return nil, StreamEvent{}, fmt.Errorf("sharpline: stream: read hello: %w", err)
	}
	frame, err := decodeServerFrame(payload)
	if err != nil {
		_ = sock.Close("bad hello")
		return nil, StreamEvent{}, err
	}
	if frame.Type == kindError {
		_ = sock.Close("rejected")
		return nil, StreamEvent{}, fmt.Errorf("%w: server refused the connection: %s (%s)",
			ErrStreamProtocol, frame.Message, frame.Code)
	}
	if frame.Type != kindHello {
		_ = sock.Close("bad hello")
		return nil, StreamEvent{}, fmt.Errorf("%w: first frame is %q, want %q",
			ErrStreamProtocol, frame.Type, kindHello)
	}

	ev, err := s.eventFor(frame)
	if err != nil {
		_ = sock.Close("bad hello")
		return nil, StreamEvent{}, err
	}

	// Only what the server did NOT restore. Asking for a channel the connection
	// already holds is answered with a `duplicate` rejection, which is correct
	// but reads on a dashboard as a client bug.
	s.mu.Lock()
	if s.err != nil {
		// Closed while this dial was in flight. Installing the socket now would
		// leak it, because nothing after Close ever looks at s.sock again.
		terminal := s.err
		s.mu.Unlock()
		_ = sock.Close("client closing")
		return nil, StreamEvent{}, terminal
	}
	want := make([]string, 0, len(s.desired))
	for _, ch := range s.desired {
		if !containsString(ev.Hello.Channels, ch) {
			want = append(want, ch)
		}
	}
	s.sock = sock
	s.connID = ev.Hello.ConnectionID
	s.requests++
	requestID := "r" + strconv.FormatUint(s.requests, 10)
	s.mu.Unlock()

	if len(want) > 0 {
		if err := s.send(ctx, clientFrame{
			Type: clientSubscribe, ID: requestID, Channels: want,
		}); err != nil {
			s.adopt(nil, "")
			_ = sock.Close("subscribe failed")
			return nil, StreamEvent{}, err
		}
	}
	return sock, ev, nil
}

// credential resolves the token to present, preferring an explicit one.
//
// It is called on every connection rather than once, so a stream that outlives
// its access token reconnects with a fresh one from the [TokenSource] instead of
// presenting an expired credential and being closed.
func (s *Stream) credential(ctx context.Context) (string, error) {
	if s.staticTok != "" {
		return s.staticTok, nil
	}
	if s.tokens == nil {
		return "", nil
	}
	token, _, err := s.tokens.AccessToken(ctx)
	if err != nil {
		// The source's error is wrapped, never the token: a TokenSource that
		// failed has nothing to hide, but this is the one call site in this file
		// that holds a credential and an error at the same time.
		return "", fmt.Errorf("sharpline: stream: access token: %w", err)
	}
	return token, nil
}

// adopt installs (or clears) the live socket.
func (s *Stream) adopt(sock StreamSocket, connID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil && sock != nil {
		// Closed while a reconnection was in flight.
		return
	}
	s.sock = sock
	s.connID = connID
}

// emit queues an event, reporting false when the stream is ending.
//
// It BLOCKS when the buffer is full, which is the whole of [StreamOptions.Buffer]'s
// argument: this client would rather become the gateway's slow consumer — an
// observable state with a metric, a resync and an event — than drop a delta and
// leave a caller holding a board it believes is current.
func (s *Stream) emit(ctx context.Context, ev StreamEvent) bool {
	if ev.ConnectionID == "" {
		ev.ConnectionID = s.ConnectionID()
	}
	select {
	case s.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// finish records the terminal error. The first one recorded wins, so a caller's
// Close is not overwritten by the read error it caused.
func (s *Stream) finish(err error) {
	if err == nil {
		err = ErrStreamClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = err
	}
	s.sock = nil
}

// -----------------------------------------------------------------------------
// Wire types
// -----------------------------------------------------------------------------

// serverFrame is one decoded server-to-client frame.
//
// Unknown fields are ACCEPTED, unlike the gateway's own decoder which refuses
// them. The asymmetry is deliberate and is the same one [Client.finish] applies
// to REST responses: a server may add a field at any time, and an SDK that
// refused the whole frame over one it did not recognise would break every
// deployed client on a purely additive change. The gateway refuses unknown
// fields because it is reading a REQUEST from an untrusted client, where the
// failure mode of tolerance is a subscription that silently does nothing.
type serverFrame struct {
	Seq  uint64     `json:"seq"`
	Type serverKind `json:"type"`
	TS   time.Time  `json:"ts"`
	ID   string     `json:"id"`

	// hello
	ConnectionID     string   `json:"connection_id"`
	Protocol         string   `json:"protocol"`
	HeartbeatSeconds int      `json:"heartbeat_seconds"`
	SessionID        string   `json:"session_id"`
	Resumed          bool     `json:"resumed"`
	Authenticated    bool     `json:"authenticated"`
	Channels         []string `json:"channels"`

	// ack
	Subscribed []string                `json:"subscribed"`
	Rejected   []RejectedStreamChannel `json:"rejected"`

	// snapshot and delta
	Channel  string            `json:"channel"`
	Markets  []json.RawMessage `json:"markets"`
	Market   json.RawMessage   `json:"market"`
	Removed  string            `json:"removed"`
	Complete bool              `json:"complete"`

	// resync
	Reason  string `json:"reason"`
	Dropped int    `json:"dropped"`
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`

	// error
	Code    StreamErrorCode `json:"code"`
	Message string          `json:"message"`
}

// serverKind is the `type` field of a server frame.
type serverKind string

const (
	kindHello    serverKind = "hello"
	kindAck      serverKind = "ack"
	kindSnapshot serverKind = "snapshot"
	kindDelta    serverKind = "delta"
	kindResync   serverKind = "resync"
	kindError    serverKind = "error"
	kindPong     serverKind = "pong"
)

// clientKind is the `type` field of a client frame.
type clientKind string

const (
	clientSubscribe   clientKind = "subscribe"
	clientUnsubscribe clientKind = "unsubscribe"
	clientResync      clientKind = "resync"
	clientPing        clientKind = "ping"
)

// clientFrame is one client-to-server message.
//
// `channels` is omitempty because a `resync` with no channels means "every
// channel this connection holds", and the gateway's decoder refuses fields it
// does not know but accepts an absent one.
type clientFrame struct {
	Type     clientKind `json:"type"`
	ID       string     `json:"id,omitempty"`
	Channels []string   `json:"channels,omitempty"`
}

// decodeServerFrame parses one inbound frame.
func decodeServerFrame(payload []byte) (serverFrame, error) {
	var f serverFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		// The payload is NOT in the error. It is untrusted input of unbounded
		// length and this error reaches a caller's log.
		return serverFrame{}, fmt.Errorf("%w: frame is not a JSON object this build reads",
			ErrStreamProtocol)
	}
	if f.Seq == 0 {
		return serverFrame{}, fmt.Errorf("%w: frame carries no sequence number", ErrStreamProtocol)
	}
	return f, nil
}

// eventFor turns a decoded frame into the event a caller sees.
func (s *Stream) eventFor(f serverFrame) (StreamEvent, error) {
	ev := StreamEvent{Seq: f.Seq, TS: f.TS, ID: f.ID, ConnectionID: s.ConnectionID()}

	switch f.Type {
	case kindHello:
		ev.Kind = StreamEventHello
		ev.ConnectionID = f.ConnectionID
		channels := f.Channels
		if channels == nil {
			channels = []string{}
		}
		ev.Hello = &StreamHello{
			ConnectionID:  f.ConnectionID,
			Protocol:      f.Protocol,
			Heartbeat:     time.Duration(f.HeartbeatSeconds) * time.Second,
			SessionID:     f.SessionID,
			Resumed:       f.Resumed,
			Authenticated: f.Authenticated,
			Channels:      channels,
		}
		if f.Protocol != StreamProtocol {
			return StreamEvent{}, fmt.Errorf("%w: hello names protocol %q, want %q",
				ErrStreamProtocol, f.Protocol, StreamProtocol)
		}

	case kindAck:
		ev.Kind = StreamEventAck
		ev.Subscribed = f.Subscribed
		ev.Rejected = f.Rejected

	case kindSnapshot:
		ev.Kind = StreamEventSnapshot
		ev.Channel = f.Channel
		ev.Markets = f.Markets
		if ev.Markets == nil {
			// An empty snapshot is a correct answer, and a nil slice would make
			// a caller distinguish "no markets" from "no field".
			ev.Markets = []json.RawMessage{}
		}

	case kindDelta:
		ev.Kind = StreamEventDelta
		ev.Channel = f.Channel
		ev.Market = f.Market
		ev.Removed = f.Removed
		if len(f.Market) == 0 && f.Removed == "" {
			return StreamEvent{}, fmt.Errorf("%w: delta carries neither a market nor a removal",
				ErrStreamProtocol)
		}

	case kindResync:
		ev.Kind = StreamEventResync
		ev.Reason = f.Reason
		ev.Gap = &StreamGap{
			From: f.FromSeq, To: f.ToSeq, Dropped: f.Dropped, ServerReported: true,
		}

	case kindError:
		ev.Kind = StreamEventError
		ev.Code = f.Code
		ev.Message = f.Message

	case kindPong:
		ev.Kind = StreamEventPong

	default:
		return StreamEvent{}, fmt.Errorf("%w: frame type %q is not one this build reads",
			ErrStreamProtocol, f.Type)
	}
	return ev, nil
}

// -----------------------------------------------------------------------------
// The default dialer
// -----------------------------------------------------------------------------

// websocketDialer is the coder/websocket implementation of [StreamDialer].
//
// It is the only place in this package that names the WebSocket library, which
// is what keeps the library out of the SDK's exported surface.
type websocketDialer struct {
	client    *http.Client
	readLimit int64
}

// DialStream implements StreamDialer.
func (d *websocketDialer) DialStream(ctx context.Context, rawURL string, subprotocols []string) (StreamSocket, error) {
	conn, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{
		HTTPClient:   d.client,
		Subprotocols: subprotocols,
		// Stated rather than left to the library's default. permessage-deflate
		// holds a flate window per connection with context takeover, and
		// compresses each small delta from a cold dictionary without it; the
		// gateway disables it for the same reasons and a negotiation neither
		// side wants is one neither side should offer.
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		// The error is returned unwrapped of the offers: coder/websocket's
		// message names the URL and the status, and the URL cannot carry a
		// credential because streamURL refuses one.
		return nil, err
	}
	conn.SetReadLimit(d.readLimit)
	return &websocketSocket{conn: conn}, nil
}

// websocketSocket adapts *websocket.Conn to [StreamSocket].
type websocketSocket struct {
	conn *websocket.Conn

	// writeMu serialises writes. coder/websocket permits one concurrent reader
	// and one concurrent writer, and [Stream] can have a caller's Subscribe and
	// its own resync request racing.
	writeMu sync.Mutex
}

// Subprotocol implements StreamSocket.
func (s *websocketSocket) Subprotocol() string { return s.conn.Subprotocol() }

// Receive implements StreamSocket.
func (s *websocketSocket) Receive(ctx context.Context) ([]byte, error) {
	typ, data, err := s.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageText {
		// The protocol is JSON text frames. A binary frame is not a frame this
		// build can read, and guessing at it would be worse than refusing.
		return nil, fmt.Errorf("%w: server sent a binary frame", ErrStreamProtocol)
	}
	return data, nil
}

// Send implements StreamSocket.
func (s *websocketSocket) Send(ctx context.Context, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.conn.Write(ctx, websocket.MessageText, payload)
}

// Close implements StreamSocket.
func (s *websocketSocket) Close(reason string) error {
	return s.conn.Close(websocket.StatusNormalClosure, truncateReason(reason))
}

// truncateReason bounds a close reason to the 123 bytes RFC 6455 allows in a
// control frame's payload, minus the two-byte status code.
func truncateReason(s string) string {
	const limit = 123
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// removeStrings returns from without the entries in remove.
//
// It allocates rather than filtering in place: the slice it is given is the
// stream's desired set, and a caller holding a slice returned by
// [Stream.Channels] must not have it rewritten underneath.
func removeStrings(from, remove []string) []string {
	out := make([]string, 0, len(from))
	for _, s := range from {
		if !containsString(remove, s) {
			out = append(out, s)
		}
	}
	return out
}

func positiveIntOr(n, fallback int) int {
	if n > 0 {
		return n
	}
	return fallback
}

func positiveInt64Or(n, fallback int64) int64 {
	if n > 0 {
		return n
	}
	return fallback
}

func positiveDurationOr(d, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

// sleepCtx waits for d, reporting false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
