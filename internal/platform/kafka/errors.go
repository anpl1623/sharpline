package kafka

import "errors"

// Sentinel errors. CLAUDE.md §12 puts domain sentinels in the domain package;
// these are platform-level, matched with errors.Is by callers and tests, and
// follow the precedent set by internal/platform/config, internal/platform/httpx
// and internal/platform/postgres.
var (
	// ErrInvalidOptions means the options could not produce a usable client.
	// Returned before any network I/O is attempted.
	ErrInvalidOptions = errors.New("kafka: invalid options")

	// ErrUnavailable means the cluster could not be reached within the
	// configured retry budget. Every attempt failed transiently.
	ErrUnavailable = errors.New("kafka: cluster unreachable")

	// ErrClosed means the client has been closed.
	ErrClosed = errors.New("kafka: client is closed")

	// ErrInvalidTopic means a topic name is empty, too long, or not a name this
	// system declares. Terraform owns topic creation (CLAUDE.md §9) and the
	// broker runs with auto-creation disabled, so a name that is wrong here
	// would otherwise surface as UNKNOWN_TOPIC_OR_PARTITION on the first
	// produce.
	ErrInvalidTopic = errors.New("kafka: invalid topic")

	// ErrInvalidProvider means a provider slug is not usable as the last
	// component of odds.raw.{provider}. The accepted charset is deliberately
	// narrower than domain.Slug's — see Provider.
	ErrInvalidProvider = errors.New("kafka: invalid provider slug")

	// ErrInvalidKey means a record key is empty or contains a byte the domain's
	// identifier charset forbids. A key chooses the partition, and on a
	// compacted topic it also identifies the snapshot entry, so a malformed key
	// is never tolerated.
	ErrInvalidKey = errors.New("kafka: invalid record key")

	// ErrInvalidMessage means a Message could not be published as given: no
	// type, an unmarshallable payload, or a type name outside the accepted
	// charset.
	ErrInvalidMessage = errors.New("kafka: invalid message")

	// ErrEmptyPayload means a caller tried to publish a message with no
	// payload. On a compacted topic a null value is a TOMBSTONE — it deletes
	// the key from the snapshot permanently — so it is never produced by
	// accident. Use the Tombstone methods, which require a written reason and
	// an explicit acknowledgement.
	ErrEmptyPayload = errors.New("kafka: payload is empty; a null value is a tombstone, use the Tombstone API")

	// ErrTombstoneNotAcknowledged means a Tombstone was passed without
	// Acknowledge set to AcknowledgeDeletesKeyFromSnapshot, or without a
	// Reason. Both are required so that a zero-valued Tombstone{} cannot delete
	// a market.
	ErrTombstoneNotAcknowledged = errors.New("kafka: tombstone not acknowledged")

	// ErrNotCompacted means a compaction-only operation — a tombstone, or a
	// snapshot read — was requested for a retention-based topic. On such a topic
	// a null value deletes nothing and a from-the-start read is whatever the
	// retention window happens to still hold, which is not a snapshot.
	ErrNotCompacted = errors.New("kafka: topic is not compacted")

	// ErrUnsupportedEnvelope means a record's envelope version is not one this
	// build understands. Refusing is deliberate: silently reading a newer
	// envelope with an older decoder is how a compacted topic that must stay
	// "replayable from scratch" (CLAUDE.md §3) gets poisoned.
	ErrUnsupportedEnvelope = errors.New("kafka: unsupported envelope version")

	// ErrMalformedEnvelope means a record's value was not a decodable envelope.
	ErrMalformedEnvelope = errors.New("kafka: malformed envelope")

	// ErrWrongKeyKind means a Delivery's key was read as the wrong domain
	// identifier — asking odds.normalized for an EventID, say. It exists so a
	// mis-keyed read fails loudly instead of producing a plausible-looking
	// identifier of the wrong kind.
	ErrWrongKeyKind = errors.New("kafka: topic is not keyed by that identifier")

	// ErrHandlerFailed wraps a handler error that stopped the consume loop
	// under ErrorPolicyStop.
	ErrHandlerFailed = errors.New("kafka: message handler failed")
)
