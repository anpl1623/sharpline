package wsgw

import "errors"

// Sentinel errors. CLAUDE.md §12 puts domain sentinels in the domain package;
// these describe gateway- and transport-level refusals that the domain has no
// opinion about, so they live with the code that returns them. Match with
// errors.Is, never on message text.
//
// Every one of them is deliberately INCURIOUS about the value that caused it.
// Client input reaches this package before anything has authenticated it, and
// an error string built from that input becomes a log line, a close reason and
// — through the metric label helpers — very nearly a Prometheus label. The
// bounded-classification discipline internal/ingest/normalizer applies to
// provider payloads applies here for the same reason and against a more
// motivated author.
var (
	// ErrInvalidOptions is returned by every constructor in this package when
	// its options do not validate. Configuration fails at construction, loudly,
	// rather than at the first connection.
	ErrInvalidOptions = errors.New("wsgw: invalid options")

	// ErrInvalidChannel means a client-supplied channel string is not one of
	// the three forms CLAUDE.md §5 defines. See channel.go; the specific
	// classification a client is told is a [RejectReason], not this error's
	// text.
	ErrInvalidChannel = errors.New("wsgw: invalid channel")

	// ErrMalformedFrame means an inbound frame was not decodable as a client
	// frame this build reads: not JSON, trailing bytes after the value, an
	// unknown field, a request id that is not printable, or a channel list that
	// violates the frame's own rules.
	//
	// It is a PROTOCOL error, not a data error: the connection is closed rather
	// than the frame skipped, because a client that cannot frame a request
	// correctly cannot be assumed to have understood the answers it already
	// received.
	ErrMalformedFrame = errors.New("wsgw: malformed client frame")

	// ErrUnknownFrameType means the frame decoded but named a `type` this build
	// does not implement. Distinct from ErrMalformedFrame because it is the one
	// protocol failure with a useful answer: the error names the supported set,
	// so a client author sees what they should have sent.
	ErrUnknownFrameType = errors.New("wsgw: unknown client frame type")

	// ErrFrameTooLarge means an inbound frame exceeded the configured ceiling.
	//
	// It is a REFUSAL, never a truncation. A truncated JSON document either
	// fails to parse — in which case the ceiling bought nothing over the
	// decoder — or, far worse, parses into a frame the client did not send.
	ErrFrameTooLarge = errors.New("wsgw: client frame exceeds the size limit")

	// ErrChannelLimit means a subscription would take a connection past
	// Options.MaxChannelsPerConnection. Also a refusal rather than a truncation:
	// silently subscribing to the first N of a client's list produces a board
	// that is missing markets with nothing anywhere saying which.
	ErrChannelLimit = errors.New("wsgw: channel limit reached for this connection")

	// ErrInvalidCredential is what credential extraction and verification
	// return when a presented token does not verify, for any reason.
	//
	// "For any reason" is the contract and it is a security requirement, not
	// laziness — the same one internal/httpapi/middleware states at its own
	// ErrInvalidCredential. An implementation MUST NOT distinguish, in anything
	// that reaches the client, between an expired token, a bad signature, an
	// unknown subject and a revoked session: each distinction is an oracle. The
	// cause belongs in the wrapped chain, which is logged and never rendered,
	// and it must never contain the token.
	ErrInvalidCredential = errors.New("wsgw: invalid credential")

	// ErrTokenInQuery means an access token was presented in the URL's query
	// string.
	//
	// It is a DISTINCT, testable outcome rather than "ignored", and D5 in doc.go
	// argues why at length: a silent downgrade to anonymous would let a client
	// keep shipping credentials in URLs and see market data arrive, which
	// teaches exactly the wrong lesson. A URL is written to access logs, sent in
	// Referer headers, kept in browser history and pasted into chat windows.
	ErrTokenInQuery = errors.New("wsgw: access token presented in the query string")

	// ErrProtocolNotOffered means the client did not offer the "sharpline.v1"
	// subprotocol. There is no default: an unversioned connection is a
	// connection whose frame shapes cannot be changed later without breaking
	// somebody silently.
	ErrProtocolNotOffered = errors.New("wsgw: client did not offer the sharpline.v1 subprotocol")

	// ErrRecordRejected means a record on price.computed could not be folded
	// into the slate: an undecodable payload, a document that fails
	// pricing.ComputedMarket.Validate, a key that disagrees with its payload, or
	// a market whose channel set cannot be derived.
	//
	// It is NOT returned to the consumer loop as a fatal error. state.go argues
	// the point: under kafka.ErrorPolicyStop a returned error stops the consumer
	// with the offset uncommitted, so one poison record on a COMPACTED topic
	// would wedge the fanout for every client for ever, and the record would be
	// redelivered for ever. It is counted and skipped instead.
	ErrRecordRejected = errors.New("wsgw: record rejected")

	// ErrUnsupportedMessage means a record on price.computed carried an envelope
	// message type this build does not read. A wiring error, not a data error.
	ErrUnsupportedMessage = errors.New("wsgw: unsupported message type")

	// ErrStaleRecord means a record was observed strictly before the state
	// already held for its market. Applying it would regress the slate and
	// publish an older price as though it were news. Skipped and counted — the
	// same guard, for the same reason, as the normalizer's and the pricer's.
	ErrStaleRecord = errors.New("wsgw: record older than the state already held")

	// ErrInvalidFrame means this package was asked to render a server frame it
	// cannot: a nil body, or a zero-valued Channel. It is a programming error in
	// the hub rather than anything a client did, and it is an error rather than
	// a panic because CLAUDE.md §12 forbids a panic outside main.
	ErrInvalidFrame = errors.New("wsgw: invalid server frame")
)
