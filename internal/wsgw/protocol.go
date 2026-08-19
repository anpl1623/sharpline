// The wire protocol: JSON text frames under the negotiated subprotocol
// "sharpline.v1".
//
// # Every frame carries a sequence number, and it is prepended, not marshalled
//
// CLAUDE.md §10 targets 10k concurrent subscribers on one node. A delta on a
// popular market therefore fans out to thousands of connections, and the naive
// implementation — marshal the frame once per connection, because the sequence
// number differs per connection — is N marshals of the same market per price
// change. That is the single decision that would make the stated target
// impossible, so the render is split in two:
//
//	[Render]      runs ONCE per publish and produces a [Body]: everything after
//	              the sequence number, from `"type"` to the closing brace.
//	[Body.Frame]  runs once per connection and prepends `{"seq":N,`.
//
// The cost per subscriber is one allocation and one copy of an already-encoded
// buffer. The Body is immutable and shared; Frame never writes into it.
//
// # The market payload is propagated BYTE FOR BYTE
//
// A snapshot's `markets` and a delta's `market` are json.RawMessage carrying the
// pricing.ComputedMarket document EXACTLY as it appeared on price.computed. It
// is never decoded-and-remarshalled and never re-shaped into a view type.
//
// internal/pricing/payload.go makes the argument in its own context and it is
// the same one here: "two mappings that must agree eventually stop agreeing,
// and the disagreement shows up as a subtly wrong line rather than as a
// failure". A gateway-specific market shape would be a second declaration of
// facts the pricer already declared, it would have to be revised in lockstep
// with pricing.SchemaVersion, and the day it fell behind the board would render
// a stale field with no error anywhere. Carrying the bytes through also means
// this package cannot accidentally round-trip a float and change a price.
//
// # The closed sets are typed constants, and the metric labels come from them
//
// [MessageKind], [ClientKind], [ErrorCode], [DropReason], [ResyncReason],
// [ConnectionResult] and [RejectReason] are defined string types with String()
// and Valid(), the way internal/ingest/normalizer defines Scope and Reason. Each
// is a Prometheus label value, so each set is closed by construction. The point
// is mechanical rather than stylistic: a label value spelled twice — once in the
// hub, once in the metrics helper — eventually differs by a character, and the
// alert rule that selects the other spelling silently matches nothing.
package wsgw

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// -----------------------------------------------------------------------------
// Protocol constants
// -----------------------------------------------------------------------------

// Protocol is the negotiated WebSocket subprotocol name, and the value echoed in
// every `hello` frame.
//
// It is versioned in the NAME rather than in a field, because RFC 6455 already
// gives the handshake a negotiation slot and a server that speaks two versions
// wants to know which one it agreed to before it writes the first byte. When
// "sharpline.v2" exists, a server offering both selects whichever the client
// listed and each connection is unambiguous for its whole life.
const Protocol = "sharpline.v1"

// Decoder bounds. These are NOT the configurable per-connection limits in
// options.go; they are the ceiling the decoder itself refuses past, so a
// malformed frame cannot make this package allocate proportionally to what a
// stranger sent.
const (
	// MaxRequestIDLen bounds the client-supplied `id` that is echoed back on
	// the answering frame.
	//
	// It is echoed, so it is untrusted output as well as untrusted input.
	// [validRequestID] additionally restricts it to printable ASCII:
	// encoding/json would escape anything else safely, but an id containing a
	// newline reaches the logs, and 64 bytes is already far more than a
	// correlation token needs.
	MaxRequestIDLen = 64

	// MaxChannelsPerFrame bounds the `channels` array in ONE client frame. It
	// is a decoder bound and is unrelated to Options.MaxChannelsPerConnection,
	// which bounds the total a connection may hold across many frames.
	MaxChannelsPerFrame = 256
)

// -----------------------------------------------------------------------------
// Closed sets
// -----------------------------------------------------------------------------

// MessageKind names a server-to-client frame. It is the `kind` label value on
// sharpline_ws_messages_sent_total, which the Grafana dashboard breaks down and
// which WebSocketResyncStorm selects as kind="delta", so the set is closed.
type MessageKind string

// The server frame kinds. Every value appears in a code path.
const (
	// KindHello is the first frame on every connection. It states the
	// connection id (which is what makes the per-connection sequence space
	// unambiguous), the negotiated protocol, the heartbeat period, whether a
	// session was resumed and what channels were restored with it.
	KindHello MessageKind = "hello"

	// KindAck answers a subscribe or unsubscribe, naming what was accepted and
	// what was refused. A partial success is reported as one, never rounded to
	// success or to failure.
	KindAck MessageKind = "ack"

	// KindSnapshot is a channel's current markets, sent once when the
	// subscription is registered. `complete` is true because the snapshot is
	// taken atomically (D2) and is never chunked; the field exists so that
	// chunking could be added later without a version bump.
	KindSnapshot MessageKind = "snapshot"

	// KindDelta is one market's change: either the new document, or a
	// `removed` market id when the market was tombstoned upstream.
	KindDelta MessageKind = "delta"

	// KindResync tells the client its stream has a hole and it must re-subscribe
	// to get a fresh snapshot. It names the reason and the sequence range that
	// was lost.
	KindResync MessageKind = "resync"

	// KindError is a bounded, coded failure. It does not necessarily close the
	// connection; the code says which.
	KindError MessageKind = "error"

	// KindPong answers an application-level `ping` frame.
	//
	// This is NOT the RFC 6455 control-frame pong that D10's idle reaping uses.
	// The two are separate on purpose: the control ping/pong is handled by the
	// WebSocket library beneath the application and tells the server the socket
	// is alive, while this one is a round trip the client can time and correlate
	// by request id. A client behind a proxy that answers control frames on its
	// behalf would look alive while receiving nothing.
	KindPong MessageKind = "pong"
)

// String implements fmt.Stringer.
func (k MessageKind) String() string { return string(k) }

// Valid reports whether k is a kind this build emits.
func (k MessageKind) Valid() bool {
	switch k {
	case KindHello, KindAck, KindSnapshot, KindDelta, KindResync, KindError, KindPong:
		return true
	default:
		return false
	}
}

// MessageKinds returns the server frame kinds in a stable order. It exists so
// the metric helper can pre-create every label value and the test can assert the
// set, rather than each writing its own list.
func MessageKinds() []MessageKind {
	return []MessageKind{KindHello, KindAck, KindSnapshot, KindDelta, KindResync, KindError, KindPong}
}

// ClientKind names a client-to-server frame.
type ClientKind string

// The client frame kinds.
const (
	// ClientSubscribe adds channels to this connection.
	ClientSubscribe ClientKind = "subscribe"

	// ClientUnsubscribe removes channels from this connection.
	ClientUnsubscribe ClientKind = "unsubscribe"

	// ClientResync asks for a fresh snapshot of the named channels. An empty
	// list means every channel this connection holds — which is what a client
	// sends after detecting a sequence gap, when it does not know which channel
	// lost a frame.
	ClientResync ClientKind = "resync"

	// ClientPing asks for a KindPong. See KindPong on why this exists beside the
	// RFC 6455 control frame.
	ClientPing ClientKind = "ping"
)

// String implements fmt.Stringer.
func (k ClientKind) String() string { return string(k) }

// Valid reports whether k is a frame this build reads.
func (k ClientKind) Valid() bool {
	switch k {
	case ClientSubscribe, ClientUnsubscribe, ClientResync, ClientPing:
		return true
	default:
		return false
	}
}

// ClientKinds returns the client frame kinds in a stable order. [DecodeClient]
// renders it into the error for an unknown type, so a client author is told what
// they should have sent instead of only that they were wrong.
func ClientKinds() []ClientKind {
	return []ClientKind{ClientSubscribe, ClientUnsubscribe, ClientResync, ClientPing}
}

// ErrorCode is the bounded classification on a KindError frame.
//
// It is what a client branches on. The accompanying message is for a human
// reading a console and is never parsed — which is why the code exists at all,
// and why no code is ever derived from an error's text.
type ErrorCode string

// The error codes.
const (
	// CodeMalformedFrame — the frame did not decode. The connection closes.
	CodeMalformedFrame ErrorCode = "malformed_frame"

	// CodeUnknownType — the frame named a type this build does not implement.
	CodeUnknownType ErrorCode = "unknown_type"

	// CodeFrameTooLarge — the frame exceeded the configured ceiling.
	CodeFrameTooLarge ErrorCode = "frame_too_large"

	// CodeInvalidChannel — one or more channels could not be parsed. Reported
	// per channel on the ack's `rejected` list; this code is for the case where
	// the frame carries nothing usable at all.
	CodeInvalidChannel ErrorCode = "invalid_channel"

	// CodeChannelLimit — the subscription would exceed
	// Options.MaxChannelsPerConnection.
	CodeChannelLimit ErrorCode = "channel_limit"

	// CodeUnauthorized — a credential was presented and did not verify, or was
	// presented in a way this gateway refuses. The connection closes; it is
	// never downgraded to anonymous (D5).
	CodeUnauthorized ErrorCode = "unauthorized"

	// CodeGoingAway — the server is shutting down (D11).
	CodeGoingAway ErrorCode = "going_away"

	// CodeInternal — a failure on this side. The message is fixed text: an
	// internal error's detail is for the log, never for the client.
	CodeInternal ErrorCode = "internal"
)

// String implements fmt.Stringer.
func (c ErrorCode) String() string { return string(c) }

// Valid reports whether c is a code this build emits.
func (c ErrorCode) Valid() bool {
	switch c {
	case CodeMalformedFrame, CodeUnknownType, CodeFrameTooLarge, CodeInvalidChannel,
		CodeChannelLimit, CodeUnauthorized, CodeGoingAway, CodeInternal:
		return true
	default:
		return false
	}
}

// DropReason is why a connection was taken down. It is the `reason` label on
// sharpline_ws_clients_dropped_total, which WebSocketClientsDropping alerts on
// by reason and which names "slow_consumer" in its own description — so the set
// is closed and these spellings are frozen.
type DropReason string

// The drop reasons.
const (
	// DropSlowConsumer — the per-client send queue overflowed and the pending
	// buffer was discarded (D4). Expected under load; a defect at idle, which
	// is exactly what the alert says.
	DropSlowConsumer DropReason = "slow_consumer"

	// DropWriteError — the socket write failed.
	DropWriteError DropReason = "write_error"

	// DropReadError — the socket read failed.
	DropReadError DropReason = "read_error"

	// DropIdleTimeout — no pong within Options.PongTimeout (D10).
	DropIdleTimeout DropReason = "idle_timeout"

	// DropProtocolError — the client sent something this build refuses:
	// a malformed frame, an unknown type, an over-long frame.
	DropProtocolError DropReason = "protocol_error"

	// DropShutdown — the server is going away (D11). Counted separately so a
	// deploy does not read as a fanout regression.
	DropShutdown DropReason = "shutdown"
)

// String implements fmt.Stringer.
func (r DropReason) String() string { return string(r) }

// Valid reports whether r is a reason this build emits.
func (r DropReason) Valid() bool {
	switch r {
	case DropSlowConsumer, DropWriteError, DropReadError, DropIdleTimeout,
		DropProtocolError, DropShutdown:
		return true
	default:
		return false
	}
}

// DropReasons returns the drop reasons in a stable order.
func DropReasons() []DropReason {
	return []DropReason{
		DropSlowConsumer, DropWriteError, DropReadError,
		DropIdleTimeout, DropProtocolError, DropShutdown,
	}
}

// ResyncReason is why a client was told to resync. It is the `reason` label on
// sharpline_ws_resyncs_total, and WebSocketResyncStorm reads the series summed
// across reasons.
type ResyncReason string

// The resync reasons.
const (
	// ResyncSlowConsumer — the buffer was discarded (D4), so the sequence
	// stream has a real, visible gap (D3).
	ResyncSlowConsumer ResyncReason = "slow_consumer"

	// ResyncClientRequested — the client sent a `resync` frame, normally
	// because it detected a gap itself.
	ResyncClientRequested ResyncReason = "client_requested"

	// ResyncPresenceLost — the Redis presence set was lost, so the fleet's view
	// of who is subscribed to what has to be rebuilt (D6). Expensive, correct,
	// and visible here rather than nowhere.
	ResyncPresenceLost ResyncReason = "presence_lost"
)

// String implements fmt.Stringer.
func (r ResyncReason) String() string { return string(r) }

// Valid reports whether r is a reason this build emits.
func (r ResyncReason) Valid() bool {
	switch r {
	case ResyncSlowConsumer, ResyncClientRequested, ResyncPresenceLost:
		return true
	default:
		return false
	}
}

// ResyncReasons returns the resync reasons in a stable order.
func ResyncReasons() []ResyncReason {
	return []ResyncReason{ResyncSlowConsumer, ResyncClientRequested, ResyncPresenceLost}
}

// ConnectionResult is the outcome of one upgrade attempt. It is the `result`
// label on sharpline_ws_connections_total, which the dashboard breaks down.
type ConnectionResult string

// The connection results.
const (
	// ConnectionAccepted — the upgrade succeeded and the connection was served.
	ConnectionAccepted ConnectionResult = "accepted"

	// ConnectionRejected — this gateway refused before upgrading: no
	// "sharpline.v1" offer, a token in the query string, a credential that did
	// not verify. Distinct from upgrade_failed because it is OUR decision, and
	// a rise in it means clients are being turned away rather than that the
	// network is broken.
	ConnectionRejected ConnectionResult = "rejected"

	// ConnectionUpgradeFailed — the handshake itself failed: not a WebSocket
	// request, a hijack that could not be performed, a client that vanished.
	ConnectionUpgradeFailed ConnectionResult = "upgrade_failed"
)

// String implements fmt.Stringer.
func (r ConnectionResult) String() string { return string(r) }

// Valid reports whether r is a result this build emits.
func (r ConnectionResult) Valid() bool {
	switch r {
	case ConnectionAccepted, ConnectionRejected, ConnectionUpgradeFailed:
		return true
	default:
		return false
	}
}

// ConnectionResults returns the connection results in a stable order.
func ConnectionResults() []ConnectionResult {
	return []ConnectionResult{ConnectionAccepted, ConnectionRejected, ConnectionUpgradeFailed}
}

// RejectReason is the bounded classification of why ONE channel in a subscribe
// request was refused. It appears on the ack's `rejected` list and as the label
// on sharpline_ws_channel_rejects_total.
//
// It is NEVER the parse error's text. A channel string is untrusted input and
// the error built from it can carry arbitrary bytes; using one as a Prometheus
// label value is a cardinality bomb whose author is a third party. This is the
// same rule, for the same reason, that internal/ingest/normalizer's Reason
// applies to provider payloads.
type RejectReason string

// The channel rejection reasons.
const (
	// RejectMalformed — the string is not `kind:id`.
	RejectMalformed RejectReason = "malformed"

	// RejectUnknownKind — the prefix is not event, market or league.
	RejectUnknownKind RejectReason = "unknown_kind"

	// RejectInvalidID — the identifier failed the domain constructor: wrong
	// charset, empty, or too long.
	RejectInvalidID RejectReason = "invalid_id"

	// RejectTooLong — the whole channel string exceeded MaxChannelLen. Checked
	// before parsing so a megabyte string is refused without being examined.
	RejectTooLong RejectReason = "too_long"

	// RejectLimitReached — the connection is already holding
	// Options.MaxChannelsPerConnection channels.
	RejectLimitReached RejectReason = "limit_reached"

	// RejectDuplicate — the connection already holds this channel. Reported
	// rather than silently ignored: a client that believes it subscribed twice
	// and unsubscribes once would otherwise expect to still be subscribed.
	RejectDuplicate RejectReason = "duplicate"
)

// String implements fmt.Stringer.
func (r RejectReason) String() string { return string(r) }

// Valid reports whether r is a reason this build emits.
func (r RejectReason) Valid() bool {
	switch r {
	case RejectMalformed, RejectUnknownKind, RejectInvalidID,
		RejectTooLong, RejectLimitReached, RejectDuplicate:
		return true
	default:
		return false
	}
}

// RejectReasons returns the channel rejection reasons in a stable order.
func RejectReasons() []RejectReason {
	return []RejectReason{
		RejectMalformed, RejectUnknownKind, RejectInvalidID,
		RejectTooLong, RejectLimitReached, RejectDuplicate,
	}
}

// -----------------------------------------------------------------------------
// Server frames
// -----------------------------------------------------------------------------

// FrameHeader is the part of a server frame every kind carries, minus the
// sequence number.
//
// The sequence number is NOT here, and that is the whole design: it is the only
// field that differs between two connections receiving the same delta, so
// keeping it out of the marshalled struct is what lets one render serve every
// subscriber. See [Render] and [Body.Frame].
type FrameHeader struct {
	// Type is the frame kind. Always present — a frame with no type would
	// render `{"seq":1,}`, which is not JSON, so [Render] checks it.
	Type MessageKind `json:"type"`

	// TS is when the server built the frame, RFC 3339 with nanoseconds (which
	// is what time.Time's JSON encoding produces).
	//
	// It is a DIAGNOSTIC and is never the staleness subtrahend. The observation
	// instant a freshness measurement is taken against lives on the price
	// itself, inside the market document, and internal/platform/kafka is
	// emphatic that the two are not interchangeable.
	TS time.Time `json:"ts"`

	// ID echoes the client's request id when this frame answers a request.
	// Absent otherwise. It is bounded and charset-restricted at decode
	// ([validRequestID]) so echoing it is safe by construction.
	ID string `json:"id,omitempty"`
}

// header returns the embedded header. It is unexported, which is what makes
// [FrameBody] a CLOSED interface: only types in this package can implement it,
// so the set of things that can reach the wire is the set declared in this file.
func (h *FrameHeader) header() *FrameHeader { return h }

// FrameBody is a renderable server frame.
//
// The unexported method is deliberate; see [FrameHeader.header]. Implementations
// are always pointers, because [Render] stamps the header in place.
type FrameBody interface {
	// Kind reports which frame this is. Render takes the type from here rather
	// than from the header, so a caller cannot render a hello frame labelled as
	// a delta.
	Kind() MessageKind

	header() *FrameHeader
}

// HelloFrame is the first frame on every connection.
type HelloFrame struct {
	FrameHeader

	// ConnectionID identifies THIS connection. It is what makes the
	// per-connection sequence space unambiguous (D3): a client that reconnects
	// sees a new connection id and resets its expected sequence to 1, so a
	// reconnect is not an epoch problem.
	ConnectionID string `json:"connection_id"`

	// Protocol is the negotiated subprotocol, echoed so a client can assert it
	// got what it offered rather than trusting the handshake it did not inspect.
	Protocol string `json:"protocol"`

	// HeartbeatSeconds is Options.PingInterval in whole seconds. The client
	// uses it to size its own liveness timer; without it the timer is a
	// hard-coded number in two places that drift.
	HeartbeatSeconds int `json:"heartbeat_seconds"`

	// SessionID is the durable session key this connection is bound to (D6).
	// Empty when the client presented none and Redis assigned none.
	SessionID string `json:"session_id,omitempty"`

	// Resumed reports that a subscription set was restored from Redis. It is
	// the field that makes affinity-free routing observable: reconnect, land on
	// another replica, see `"resumed": true` and the channels come back.
	Resumed bool `json:"resumed"`

	// Authenticated reports whether a credential was presented AND verified.
	// False is the normal case: market data is public (D5).
	Authenticated bool `json:"authenticated"`

	// Channels are the restored channels. Empty — and rendered as `[]`, never
	// null — when nothing was restored. An empty set is a correct answer, not a
	// missing one.
	Channels []Channel `json:"channels"`
}

// Kind implements FrameBody.
func (f *HelloFrame) Kind() MessageKind { return KindHello }

// RejectedChannel is one refused entry on an ack.
type RejectedChannel struct {
	// Channel is the client's own string, echoed BOUNDED — see [SafeEcho]. It
	// is echoed at all because a client that sent forty channels needs to know
	// which one was refused, and it is bounded because the value is untrusted
	// and lands in a log line.
	Channel string `json:"channel"`

	// Reason is the bounded classification. Never the parse error's text.
	Reason RejectReason `json:"reason"`
}

// AckFrame answers a subscribe or an unsubscribe.
//
// A partial success is reported as one. Rounding it to success would leave the
// client believing it holds a channel it does not; rounding it to failure would
// throw away the channels that were fine.
type AckFrame struct {
	FrameHeader

	// Subscribed is what the connection now holds as a result of this request.
	// Rendered as `[]` when empty.
	Subscribed []Channel `json:"subscribed"`

	// Rejected is what was refused, with a bounded reason each. Rendered as
	// `[]` when empty.
	Rejected []RejectedChannel `json:"rejected"`
}

// Kind implements FrameBody.
func (f *AckFrame) Kind() MessageKind { return KindAck }

// SnapshotFrame is a channel's current markets.
type SnapshotFrame struct {
	FrameHeader

	// Channel is the channel this snapshot is for.
	Channel Channel `json:"channel"`

	// Markets are the pricing.ComputedMarket documents, byte for byte as they
	// appeared on price.computed. An EMPTY array is a correct snapshot of a
	// channel with no markets — a league with no scheduled events, an event
	// whose markets have all been tombstoned — and it is rendered as `[]`.
	// Nothing here fabricates a placeholder to make it look populated.
	Markets []json.RawMessage `json:"markets"`

	// Complete reports that this frame is the whole snapshot. It is always true
	// today, because the snapshot is taken atomically under the state lock (D2)
	// and is never chunked. The field exists so chunking could be introduced
	// without a protocol version bump: a client written today already checks it.
	Complete bool `json:"complete"`
}

// Kind implements FrameBody.
func (f *SnapshotFrame) Kind() MessageKind { return KindSnapshot }

// DeltaFrame is one market's change on one channel.
//
// It has two shapes and exactly one of them is populated: an UPDATE carries
// Market, and a TOMBSTONE carries Removed. They are one frame kind rather than
// two because a client's handler for both is "replace what you hold for this
// market id", and splitting them would let a client implement one and forget
// the other — which presents as a settled market that never leaves the board.
type DeltaFrame struct {
	FrameHeader

	// Channel is the channel this delta is being delivered on. The same market
	// is delivered on up to three channels (see [ChannelsFor]); the field is
	// what lets a client that holds two of them attribute the frame.
	Channel Channel `json:"channel"`

	// Market is the new document, byte for byte. Absent on a tombstone.
	Market json.RawMessage `json:"market,omitempty"`

	// Removed is the market id that no longer exists. Absent on an update.
	//
	// A tombstone on a compacted topic means the market is gone for ever, so a
	// client that ignores this leaves it on the board permanently — no further
	// record for that key is coming. internal/platform/kafka states the same
	// obligation for its own consumers.
	Removed string `json:"removed,omitempty"`
}

// Kind implements FrameBody.
func (f *DeltaFrame) Kind() MessageKind { return KindDelta }

// ResyncFrame tells the client its stream has a hole.
type ResyncFrame struct {
	FrameHeader

	// Reason is the bounded classification.
	Reason ResyncReason `json:"reason"`

	// Dropped is how many frames were discarded.
	Dropped int `json:"dropped"`

	// FromSeq and ToSeq bracket the sequence numbers the client did not
	// receive, inclusive. They are what make D3 legible from a browser console:
	// the client can check them against the last sequence it actually saw and
	// confirm the server and it agree about the size of the hole.
	FromSeq uint64 `json:"from_seq"`
	ToSeq   uint64 `json:"to_seq"`
}

// Kind implements FrameBody.
func (f *ResyncFrame) Kind() MessageKind { return KindResync }

// ErrorFrame is a bounded, coded failure.
type ErrorFrame struct {
	FrameHeader

	// Code is what a client branches on.
	Code ErrorCode `json:"code"`

	// Message is fixed, human-readable text for a console. It NEVER contains
	// the client's input verbatim and never contains an internal error's
	// detail: the first is untrusted output, the second is an oracle.
	Message string `json:"message"`
}

// Kind implements FrameBody.
func (f *ErrorFrame) Kind() MessageKind { return KindError }

// PongFrame answers a client `ping`.
type PongFrame struct {
	FrameHeader
}

// Kind implements FrameBody.
func (f *PongFrame) Kind() MessageKind { return KindPong }

// -----------------------------------------------------------------------------
// Server frame constructors
// -----------------------------------------------------------------------------
//
// They exist for one reason beyond convenience: a nil slice marshals to `null`
// and an empty one to `[]`, and a client that must special-case null where it
// expected an array is a client that will eventually forget to. Every
// constructor normalises its slices, so an empty result is always `[]`.

// NewHello builds the opening frame. heartbeat is rounded down to whole seconds
// because the field is an integer and a client sizing a timer from it should
// never get a period longer than the server's own.
func NewHello(connectionID, sessionID string, heartbeat time.Duration, resumed, authenticated bool, channels []Channel) *HelloFrame {
	if channels == nil {
		channels = []Channel{}
	}
	return &HelloFrame{
		ConnectionID:     connectionID,
		Protocol:         Protocol,
		HeartbeatSeconds: int(heartbeat / time.Second),
		SessionID:        sessionID,
		Resumed:          resumed,
		Authenticated:    authenticated,
		Channels:         channels,
	}
}

// NewAck builds a subscribe/unsubscribe acknowledgement.
func NewAck(subscribed []Channel, rejected []RejectedChannel) *AckFrame {
	if subscribed == nil {
		subscribed = []Channel{}
	}
	if rejected == nil {
		rejected = []RejectedChannel{}
	}
	return &AckFrame{Subscribed: subscribed, Rejected: rejected}
}

// NewSnapshot builds a channel snapshot. A nil markets slice becomes `[]`,
// which is the correct rendering of a channel that currently holds nothing.
func NewSnapshot(ch Channel, markets []json.RawMessage) *SnapshotFrame {
	if markets == nil {
		markets = []json.RawMessage{}
	}
	return &SnapshotFrame{Channel: ch, Markets: markets, Complete: true}
}

// NewDelta builds an update delta carrying the market document verbatim.
func NewDelta(ch Channel, market json.RawMessage) *DeltaFrame {
	return &DeltaFrame{Channel: ch, Market: market}
}

// NewRemoval builds a tombstone delta.
func NewRemoval(ch Channel, marketID string) *DeltaFrame {
	return &DeltaFrame{Channel: ch, Removed: marketID}
}

// NewResync builds a resync instruction.
func NewResync(reason ResyncReason, dropped int, fromSeq, toSeq uint64) *ResyncFrame {
	return &ResyncFrame{Reason: reason, Dropped: dropped, FromSeq: fromSeq, ToSeq: toSeq}
}

// NewError builds a coded error frame.
func NewError(code ErrorCode, message string) *ErrorFrame {
	return &ErrorFrame{Code: code, Message: message}
}

// NewPong builds a pong.
func NewPong() *PongFrame { return &PongFrame{} }

// -----------------------------------------------------------------------------
// The shared-body render
// -----------------------------------------------------------------------------

// seqPrefix is the opening of every framed message. Declared once so the two
// places that reason about it — [Body.Frame] and its test — cannot disagree.
const seqPrefix = `{"seq":`

// Body is a rendered server frame MINUS its sequence number: the bytes from
// `"type"` through the closing brace, with no leading `{`.
//
// It is IMMUTABLE and SHARED. One Body is rendered per publish and handed to
// every subscriber's send queue; [Body.Frame] copies rather than writing into
// it. Anything that mutated a Body would corrupt every other connection's frame,
// so nothing does.
type Body []byte

// Frame prepends the sequence number and returns the complete JSON frame.
//
// This is the per-connection half of the split argued at the top of this file:
// it is one allocation and one copy of an already-encoded buffer, which is what
// keeps a delta on a popular market from costing N marshals.
func (b Body) Frame(seq uint64) []byte {
	out := make([]byte, 0, len(seqPrefix)+20+1+len(b))
	out = append(out, seqPrefix...)
	out = strconv.AppendUint(out, seq, 10)
	out = append(out, ',')
	return append(out, b...)
}

// Render encodes a server frame once, for every connection that will receive it.
//
// ts stamps FrameHeader.TS and id echoes the client's request id (empty when the
// frame answers no request). The frame's `type` is taken from [FrameBody.Kind],
// not from the header, so a mislabelled frame is unrepresentable.
//
// The implementation marshals the whole struct and drops the leading `{`. That
// is exact rather than clever: encoding/json always renders a struct as an
// object, [FrameHeader.Type] is never omitted, so the result always begins
// `{"type":` and always ends `}`. Both facts are asserted before the slice is
// taken, and protocol_test.go round-trips the assembled frame through
// encoding/json to prove the seam produces valid JSON with no doubled comma and
// no missing brace.
func Render(b FrameBody, ts time.Time, id string) (Body, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: nil frame body", ErrInvalidFrame)
	}
	kind := b.Kind()
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: %q is not a server frame kind this build emits", ErrInvalidFrame, kind)
	}

	h := b.header()
	h.Type = kind
	h.TS = ts.UTC()
	h.ID = id

	raw, err := json.Marshal(b)
	if err != nil {
		// A Channel that was never parsed refuses to marshal; that is the
		// expected cause here and it is a hub bug, not a client one.
		return nil, fmt.Errorf("%w: marshal %s frame: %w", ErrInvalidFrame, kind, err)
	}
	if len(raw) < 2 || raw[0] != '{' || raw[len(raw)-1] != '}' {
		return nil, fmt.Errorf("%w: %s frame did not marshal as a JSON object", ErrInvalidFrame, kind)
	}
	return Body(raw[1:]), nil
}

// Frame renders a frame and stamps a sequence number in one step.
//
// It is the convenience path for a frame with exactly one recipient — hello,
// ack, pong, resync, error. A delta or a snapshot with many recipients calls
// [Render] once and [Body.Frame] per connection instead; that is the whole
// point of the split, so this function deliberately does not take a connection
// list.
func Frame(seq uint64, b FrameBody, ts time.Time, id string) ([]byte, error) {
	body, err := Render(b, ts, id)
	if err != nil {
		return nil, err
	}
	return body.Frame(seq), nil
}

// -----------------------------------------------------------------------------
// Client frames
// -----------------------------------------------------------------------------

// ClientFrame is one decoded client-to-server message.
//
// Channels are RAW STRINGS here, not [Channel] values, and that separation is
// deliberate: decoding is about the frame's shape and parsing is about the
// channel grammar, and a subscribe naming three good channels and one bad one
// must produce three subscriptions and one bounded rejection rather than a
// failed decode. [ParseChannel] is applied afterwards, per entry.
type ClientFrame struct {
	Type     ClientKind `json:"type"`
	ID       string     `json:"id,omitempty"`
	Channels []string   `json:"channels,omitempty"`
}

// DecodeClient parses one inbound frame, defensively.
//
// maxBytes bounds the frame; zero or negative means [DefaultMaxFrameBytes]. The
// bound is checked BEFORE the decoder runs, so an over-long frame is refused
// without being examined at all.
//
// # Unknown fields are refused, and that is a considered trade
//
// json.Decoder.DisallowUnknownFields is on. The cost is that a client sending a
// field this build does not know is rejected rather than tolerated, which is the
// opposite of the forward-compatibility rule internal/platform/kafka applies to
// its envelope. The reason the trade goes the other way here: an envelope is
// read by a consumer that must survive a newer producer, while this is a REQUEST
// from an untrusted client, and the failure mode of tolerance is silent. A
// client sending `{"type":"subscribe","channel":"market:x"}` — singular, a typo
// away from correct — would otherwise subscribe to nothing, receive an ack
// listing nothing, and its author would spend an afternoon on it. The protocol
// is versioned in the subprotocol name, so a client that needs a new field
// negotiates "sharpline.v2" and gets a server that knows about it.
//
// Trailing bytes after the JSON value are refused for the same reason: two
// values in one frame means the client and the server disagree about framing,
// and guessing which one was meant is not a recovery.
func DecodeClient(data []byte, maxBytes int) (ClientFrame, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	if len(data) > maxBytes {
		// The frame itself is NOT in the error. It is untrusted input of
		// unbounded length and this error becomes a log line.
		return ClientFrame{}, fmt.Errorf("%w: %d bytes, limit is %d", ErrFrameTooLarge, len(data), maxBytes)
	}

	var f ClientFrame
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		// json's error can quote the offending input, so it is not wrapped into
		// anything that reaches a client. The classification is what travels.
		return ClientFrame{}, fmt.Errorf("%w: not a client frame this build reads", ErrMalformedFrame)
	}
	if dec.More() {
		return ClientFrame{}, fmt.Errorf("%w: trailing bytes after the JSON value", ErrMalformedFrame)
	}

	if !f.Type.Valid() {
		// The supported set is named. This is the one protocol failure with a
		// genuinely useful answer, and the value the client sent is NOT echoed
		// — the list of what is accepted is enough to fix the mistake.
		return ClientFrame{}, fmt.Errorf("%w: supported types are %s", ErrUnknownFrameType, clientKindList())
	}
	if !validRequestID(f.ID) {
		return ClientFrame{}, fmt.Errorf("%w: id must be at most %d printable ASCII bytes",
			ErrMalformedFrame, MaxRequestIDLen)
	}
	if len(f.Channels) > MaxChannelsPerFrame {
		return ClientFrame{}, fmt.Errorf("%w: %d channels in one frame, limit is %d",
			ErrMalformedFrame, len(f.Channels), MaxChannelsPerFrame)
	}

	switch f.Type {
	case ClientSubscribe, ClientUnsubscribe:
		// An empty list is refused rather than treated as a no-op: a client that
		// sent one believes it asked for something, and an ack listing nothing
		// would look like a server that lost the request.
		if len(f.Channels) == 0 {
			return ClientFrame{}, fmt.Errorf("%w: %s requires at least one channel", ErrMalformedFrame, f.Type)
		}
	case ClientPing:
		if len(f.Channels) != 0 {
			return ClientFrame{}, fmt.Errorf("%w: ping carries no channels", ErrMalformedFrame)
		}
	case ClientResync:
		// An empty list IS meaningful here: every channel this connection
		// holds. It is what a client sends on detecting a sequence gap, when it
		// cannot know which channel lost a frame.
	}
	return f, nil
}

// clientKindList renders the supported client types for an error message.
func clientKindList() string {
	kinds := ClientKinds()
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = k.String()
	}
	return strings.Join(names, ", ")
}

// validRequestID reports whether an echoed request id is safe and bounded.
//
// Printable ASCII only. encoding/json would escape anything else correctly on
// the way back out, so this is not about injection into the frame — it is about
// the id reaching a log line and a span attribute, where a control byte or a
// megabyte of text is a problem the JSON encoder does not solve.
func validRequestID(id string) bool {
	if len(id) > MaxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

// SafeEcho bounds an untrusted string for inclusion in a frame the client will
// read back, or in a log line.
//
// It is the analogue of internal/domain's `sample`, and it exists for the same
// reason: a client needs to know WHICH of its forty channels was refused, so
// something has to be echoed, and echoing an arbitrary-length byte string is how
// a log becomes an attack surface. Bytes outside printable ASCII become '?',
// because dropping them would let two different inputs echo identically and a
// client would be told a channel it never sent was refused.
func SafeEcho(s string) string {
	const limit = 64
	truncated := false
	if len(s) > limit {
		s, truncated = s[:limit], true
	}
	out := make([]byte, 0, len(s)+1)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		out = append(out, c)
	}
	if truncated {
		out = append(out, '~')
	}
	return string(out)
}

// ErrorCodeFor maps a protocol failure onto the code a client is told.
//
// It is a switch over SENTINELS, never over error text, and its default is
// [CodeInternal] rather than a guess — an unclassified failure is this
// package's problem to fix, and telling the client it was their fault would
// send a client author looking in the wrong place.
func ErrorCodeFor(err error) ErrorCode {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrFrameTooLarge):
		return CodeFrameTooLarge
	case errors.Is(err, ErrUnknownFrameType):
		return CodeUnknownType
	case errors.Is(err, ErrMalformedFrame):
		return CodeMalformedFrame
	case errors.Is(err, ErrChannelLimit):
		return CodeChannelLimit
	case errors.Is(err, ErrInvalidChannel):
		return CodeInvalidChannel
	case errors.Is(err, ErrInvalidCredential), errors.Is(err, ErrTokenInQuery),
		errors.Is(err, ErrProtocolNotOffered):
		return CodeUnauthorized
	default:
		return CodeInternal
	}
}

// DropReasonFor maps a protocol failure onto the metric label a dropped
// connection is counted under. Every protocol-level refusal is one reason —
// "the client sent something we refuse" — because splitting it further would
// put a client-controlled distinction into a label set.
func DropReasonFor(err error) DropReason {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrFrameTooLarge), errors.Is(err, ErrUnknownFrameType),
		errors.Is(err, ErrMalformedFrame), errors.Is(err, ErrInvalidChannel),
		errors.Is(err, ErrChannelLimit):
		return DropProtocolError
	default:
		return DropReadError
	}
}
