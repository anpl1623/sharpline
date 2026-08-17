// The wire format: a versioned JSON envelope in the record value, plus a small
// set of record headers that also describe a record with no value at all.
//
// The choice of JSON, its costs and its mitigations are argued in doc.go. This
// file is the mechanism.
//
// # Why the metadata appears twice
//
// A tombstone has a NULL VALUE by definition — that is what makes the broker
// delete the key — so nothing about it can be carried in the body. If the only
// description of a record lived in the envelope, a tombstone would be an
// anonymous deletion: no producer, no reason, no observation instant, nothing
// for an operator staring at kafka-ui to go on.
//
// So the envelope's identifying fields are ALSO written as record headers, and a
// tombstone carries the headers alone. The duplication is a few dozen bytes per
// record and it buys a self-describing deletion. It is one-directional: the
// envelope is authoritative for a record that has one, and Delivery reads the
// headers only where the envelope is absent.
package kafka

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/anpl1623/sharpline/internal/domain"
)

// EnvelopeVersion is the version this build writes.
//
// Every record carries it, and a decoder refuses a version it does not
// recognise (ErrUnsupportedEnvelope) rather than reading a newer record with an
// older reader. CLAUDE.md §3 requires a compacted topic to stay "replayable from
// scratch"; a decoder that silently mis-reads half the fields of a v2 record
// turns a replay into a corruption, and it does it quietly, months after the
// change.
//
// The forward-compatibility rules that keep this at 1 for as long as possible:
//
//	ADDING an optional field to a payload is NOT a version bump. encoding/json
//	ignores unknown fields on decode, so an old consumer skips it.
//
//	REMOVING a field, RENAMING one, or CHANGING the meaning or unit of one IS a
//	version bump, because an old consumer would read it and be wrong.
//
// When 2 arrives, MinSupportedEnvelopeVersion is what decides whether the v1
// records already on a compacted topic remain readable. Bumping the minimum is
// a decision to abandon them, and it needs the same care as a migration.
const (
	EnvelopeVersion             = 1
	MinSupportedEnvelopeVersion = 1
)

// Record header keys.
//
// The `sharpline-` prefix keeps them clear of the W3C tracing headers, which are
// unprefixed by specification. Lowercase because Kafka header keys are
// byte-compared, not folded, and one convention avoids a class of near-miss bug.
const (
	// HeaderEnvelopeVersion is the decimal envelope version. Present on every
	// record including tombstones.
	HeaderEnvelopeVersion = "sharpline-v"

	// HeaderMessageType is the payload type name, e.g. "odds.normalized.v1".
	HeaderMessageType = "sharpline-type"

	// HeaderProducer is the name of the service that produced the record.
	HeaderProducer = "sharpline-producer"

	// HeaderMessageID is the producer-assigned message id, when there is one.
	HeaderMessageID = "sharpline-id"

	// HeaderObservedAt is the provider observation instant in RFC 3339 with
	// nanoseconds, when the message carries one.
	//
	// This is the value CLAUDE.md §9's headline staleness SLO is measured
	// against, and the phase-2 handoff is emphatic that it is NOT interchangeable
	// with ingested_at or with updated_at. It is propagated UNCHANGED through
	// every hop: ingest stamps it from the provider's own last_update, and
	// neither the normalizer, the pricer nor the fanout hub re-stamps it.
	HeaderObservedAt = "sharpline-observed-at"

	// HeaderTombstone marks a deletion. Present only on tombstones, with value
	// "1".
	HeaderTombstone = "sharpline-tombstone"

	// HeaderTombstoneReason carries the operator-supplied reason for a deletion.
	// Required by the Tombstone API, so it is always present on a tombstone this
	// package produced.
	HeaderTombstoneReason = "sharpline-tombstone-reason"
)

// tombstoneHeaderValue is the value HeaderTombstone carries.
const tombstoneHeaderValue = "1"

// maxMessageTypeLen bounds the type name. It is a Prometheus label value and a
// span attribute, so it is bounded by construction.
const maxMessageTypeLen = 64

// Envelope is the JSON document in a non-tombstone record's value.
//
// Field names are short because they repeat on every record and JSON has no
// schema to hoist them into. `omitzero` (Go 1.24+) drops a zero time.Time
// rather than emitting "0001-01-01T00:00:00Z", which would otherwise be
// indistinguishable from a real-but-wrong timestamp when read in kafka-ui.
type Envelope struct {
	// Version is EnvelopeVersion as written by the producer.
	Version int `json:"v"`

	// Type names the payload shape, e.g. "odds.normalized.v1". It is what a
	// consumer switches on, and it is versioned independently of the envelope:
	// the envelope version governs the frame, the type governs the contents.
	Type string `json:"type"`

	// ID is a producer-assigned identifier, used by consumers that need to
	// deduplicate across a redelivery. Optional — at-least-once delivery is
	// usually made idempotent by a database constraint rather than by carrying
	// an id (see doc.go), so this is for the cases where it is not.
	ID string `json:"id,omitempty"`

	// Producer is the name of the service that wrote the record.
	Producer string `json:"producer"`

	// ProducedAt is when this process built the record. It is a diagnostic —
	// the difference between it and ObservedAt is provider-attributable
	// staleness — and it is never the subtrahend in the staleness SLO.
	ProducedAt time.Time `json:"produced_at"`

	// ObservedAt is the provider's own observation instant, propagated
	// unchanged. Zero when the message is not derived from a provider
	// observation (a wager event, say).
	ObservedAt time.Time `json:"observed_at,omitzero"`

	// Data is the payload, left raw so that the bus layer never has to know a
	// domain type and a consumer pays for exactly one unmarshal of the shape it
	// actually wants.
	Data json.RawMessage `json:"data"`
}

// Validate reports whether e is a usable envelope.
func (e Envelope) Validate() error {
	switch {
	case e.Version < MinSupportedEnvelopeVersion || e.Version > EnvelopeVersion:
		return fmt.Errorf("%w: %d (this build reads %d..%d)",
			ErrUnsupportedEnvelope, e.Version, MinSupportedEnvelopeVersion, EnvelopeVersion)
	case e.Type == "":
		return fmt.Errorf("%w: no type", ErrMalformedEnvelope)
	case len(e.Data) == 0:
		return fmt.Errorf("%w: no data", ErrMalformedEnvelope)
	}
	return nil
}

// Message is what a caller publishes.
//
// It is deliberately not the Envelope: the version, the producer name and the
// production instant are the bus layer's business, not the caller's, and a
// caller that could set them could get them wrong.
type Message struct {
	// Type names the payload shape and is required. Charset is [a-z0-9._-],
	// because it becomes a Prometheus label value and a span attribute.
	Type string

	// ID is an optional producer-assigned identifier for cross-redelivery
	// deduplication.
	ID string

	// ObservedAt is the provider observation instant, where the message derives
	// from one. Leave it zero otherwise; do NOT set it to time.Now as a
	// placeholder, because that produces a staleness measurement of zero and
	// makes the headline SLO report perfect health for data that has none.
	ObservedAt time.Time

	// Payload is marshalled to JSON as the envelope's data. A json.RawMessage
	// passes through unchanged, so a caller that already has bytes pays no
	// second marshal.
	Payload any
}

// validate checks a Message before any I/O.
func (m Message) validate() error {
	if err := validateMessageType(m.Type); err != nil {
		return err
	}
	if m.Payload == nil {
		return ErrEmptyPayload
	}
	if raw, ok := m.Payload.(json.RawMessage); ok && len(raw) == 0 {
		return ErrEmptyPayload
	}
	if len(m.ID) > domain.MaxIDLen {
		return fmt.Errorf("%w: id is %d bytes, limit is %d", ErrInvalidMessage, len(m.ID), domain.MaxIDLen)
	}
	return nil
}

// validateMessageType enforces the type-name charset.
func validateMessageType(t string) error {
	if t == "" {
		return fmt.Errorf("%w: no type", ErrInvalidMessage)
	}
	if len(t) > maxMessageTypeLen {
		return fmt.Errorf("%w: type %q is %d bytes, limit is %d",
			ErrInvalidMessage, t, len(t), maxMessageTypeLen)
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return fmt.Errorf("%w: type %q contains %q; allowed charset is [a-z0-9._-]",
				ErrInvalidMessage, t, string(rune(c)))
		}
	}
	return nil
}

// encode builds the envelope and its JSON encoding.
func (m Message) encode(producer string, producedAt time.Time) ([]byte, Envelope, error) {
	if err := m.validate(); err != nil {
		return nil, Envelope{}, err
	}

	data, err := json.Marshal(m.Payload)
	if err != nil {
		// The payload may embed user data; json's error names the offending Go
		// type, not the value, so this is safe to surface.
		return nil, Envelope{}, fmt.Errorf("%w: marshal payload: %w", ErrInvalidMessage, err)
	}
	// json.Marshal(nil-valued interface) yields "null", four bytes that would
	// pass a length check and decode into nothing. Caught here rather than at
	// the consumer, which cannot tell it from a deliberate null payload.
	if string(data) == "null" {
		return nil, Envelope{}, ErrEmptyPayload
	}

	env := Envelope{
		Version:    EnvelopeVersion,
		Type:       m.Type,
		ID:         m.ID,
		Producer:   producer,
		ProducedAt: producedAt.UTC(),
		ObservedAt: m.ObservedAt.UTC(),
		Data:       data,
	}
	// A zero ObservedAt must stay zero through UTC() so `omitzero` drops it.
	if m.ObservedAt.IsZero() {
		env.ObservedAt = time.Time{}
	}

	value, err := json.Marshal(env)
	if err != nil {
		return nil, Envelope{}, fmt.Errorf("%w: marshal envelope: %w", ErrInvalidMessage, err)
	}
	return value, env, nil
}

// headers builds the descriptive record headers for a non-tombstone record.
// Capacity leaves room for the two W3C tracing headers the propagator appends.
func (e Envelope) headers() []kgo.RecordHeader {
	h := make([]kgo.RecordHeader, 0, 7)
	h = append(h,
		kgo.RecordHeader{Key: HeaderEnvelopeVersion, Value: []byte(strconv.Itoa(e.Version))},
		kgo.RecordHeader{Key: HeaderMessageType, Value: []byte(e.Type)},
		kgo.RecordHeader{Key: HeaderProducer, Value: []byte(e.Producer)},
	)
	if e.ID != "" {
		h = append(h, kgo.RecordHeader{Key: HeaderMessageID, Value: []byte(e.ID)})
	}
	if !e.ObservedAt.IsZero() {
		h = append(h, kgo.RecordHeader{
			Key:   HeaderObservedAt,
			Value: []byte(e.ObservedAt.UTC().Format(time.RFC3339Nano)),
		})
	}
	return h
}

// Tombstone describes the permanent deletion of a key from a compacted topic.
//
// It is awkward on purpose. A tombstone on odds.normalized removes a market from
// the snapshot every client builds on connect, and after the topic's
// delete.retention.ms elapses the tombstone itself is collected, so the deletion
// stops being visible in the log and becomes indistinguishable from "that market
// never existed". There is no undo short of re-publishing the market.
//
// So a zero-valued Tombstone{} is rejected: both a written Reason and the
// Acknowledge constant are required. The cost is a line of ceremony at the four
// or five call sites that legitimately delete a market — a market suspended by
// the admin console, an event that has settled and been swept — and the benefit
// is that no accidental empty payload can ever become one.
type Tombstone struct {
	// Reason is required and free text. It is written to
	// HeaderTombstoneReason and logged, so an operator reading kafka-ui six
	// months later can tell a deliberate sweep from a bug.
	Reason string

	// Acknowledge must be AcknowledgeDeletesKeyFromSnapshot. Its only purpose
	// is to make the zero value invalid.
	Acknowledge TombstoneAcknowledgement

	// ObservedAt is the instant the deletion was decided, where that is known
	// (an event's settlement time, a suspension timestamp). Optional.
	ObservedAt time.Time
}

// TombstoneAcknowledgement is a single-valued type whose only member is
// AcknowledgeDeletesKeyFromSnapshot.
type TombstoneAcknowledgement string

// AcknowledgeDeletesKeyFromSnapshot is the value Tombstone.Acknowledge must
// carry. It is spelled out rather than being a bool because `true` at a call
// site says nothing, and this string is greppable: `grep -r
// AcknowledgeDeletesKeyFromSnapshot` enumerates every place in the repository
// that can delete a market.
const AcknowledgeDeletesKeyFromSnapshot TombstoneAcknowledgement = "deletes-key-from-snapshot"

// validate checks a Tombstone before any I/O.
func (t Tombstone) validate() error {
	if t.Acknowledge != AcknowledgeDeletesKeyFromSnapshot {
		return fmt.Errorf("%w: set Acknowledge to kafka.AcknowledgeDeletesKeyFromSnapshot",
			ErrTombstoneNotAcknowledged)
	}
	if t.Reason == "" {
		return fmt.Errorf("%w: Reason is required", ErrTombstoneNotAcknowledged)
	}
	if len(t.Reason) > 512 {
		return fmt.Errorf("%w: Reason is %d bytes, limit is 512",
			ErrTombstoneNotAcknowledged, len(t.Reason))
	}
	return nil
}

// headers builds the record headers for a tombstone. This is the only
// description a deletion carries, because its value is null.
func (t Tombstone) headers(producer string) []kgo.RecordHeader {
	h := make([]kgo.RecordHeader, 0, 7)
	h = append(h,
		kgo.RecordHeader{Key: HeaderEnvelopeVersion, Value: []byte(strconv.Itoa(EnvelopeVersion))},
		kgo.RecordHeader{Key: HeaderProducer, Value: []byte(producer)},
		kgo.RecordHeader{Key: HeaderTombstone, Value: []byte(tombstoneHeaderValue)},
		kgo.RecordHeader{Key: HeaderTombstoneReason, Value: []byte(t.Reason)},
	)
	if !t.ObservedAt.IsZero() {
		h = append(h, kgo.RecordHeader{
			Key:   HeaderObservedAt,
			Value: []byte(t.ObservedAt.UTC().Format(time.RFC3339Nano)),
		})
	}
	return h
}

// Delivery is one consumed record: its Kafka coordinates, its headers, and
// either a decoded envelope or the fact that it is a tombstone.
//
// It is a distinct type from Message because the two are not symmetric. A
// Delivery knows its partition, offset and broker timestamp; a Message cannot.
// A Delivery may be a deletion; a Message never is.
type Delivery struct {
	// Kafka coordinates. Offset and Partition are what a resume, a lag
	// calculation and a bug report are all expressed in.
	Topic     string
	Partition int32
	Offset    int64

	// Key is the record key as a string. Read it through MarketID, EventID or
	// WagerID rather than parsing it here: those methods check the topic's
	// declared key kind, so asking odds.normalized for an EventID fails instead
	// of returning a plausible identifier of the wrong sort.
	Key string

	// Timestamp is the record's own timestamp. Records are produced with
	// CreateTime semantics, so this is the producer's clock, not the broker's.
	// It is NOT the staleness subtrahend — ObservedAt is.
	Timestamp time.Time

	// Headers are the record's headers, flattened. A repeated key keeps its
	// first occurrence; nothing this package writes repeats a key.
	Headers map[string]string

	// Envelope is the decoded envelope. Zero when Tombstone is true.
	Envelope Envelope

	// Tombstone reports that this record is a deletion of Key.
	//
	// A consumer of a compacted topic MUST handle it. Ignoring a tombstone
	// leaves a deleted market in whatever cache or table the consumer maintains,
	// and it stays there for ever, because no further record for that key is
	// coming.
	Tombstone bool

	// TombstoneReason is the producer's stated reason, when the record was
	// produced through this package's Tombstone API. Empty for a tombstone
	// written by anything else.
	TombstoneReason string

	// value is the raw record value, retained for a debug log and for a
	// consumer that wants the bytes. Not exported as a field so it cannot be
	// mutated under the Delivery.
	value []byte
}

// Value returns the raw record value. Nil for a tombstone.
//
// The returned slice aliases the fetch buffer; do not retain or mutate it past
// the handler's return.
func (d *Delivery) Value() []byte { return d.value }

// Unmarshal decodes the envelope's payload into v.
//
// This is the normal way to read a Delivery. It returns ErrMalformedEnvelope for
// a tombstone, because a deletion has no payload and a caller that unmarshals
// one without checking Tombstone first has a bug that would otherwise present as
// an empty struct.
func (d *Delivery) Unmarshal(v any) error {
	if d.Tombstone {
		return fmt.Errorf("%w: record at %s/%d offset %d is a tombstone and has no payload",
			ErrMalformedEnvelope, d.Topic, d.Partition, d.Offset)
	}
	if err := json.Unmarshal(d.Envelope.Data, v); err != nil {
		return fmt.Errorf("%w: unmarshal %s payload at %s/%d offset %d: %w",
			ErrMalformedEnvelope, d.Envelope.Type, d.Topic, d.Partition, d.Offset, err)
	}
	return nil
}

// ObservedAt returns the provider observation instant this record carries, and
// whether it has one.
//
// The envelope is authoritative. The header is consulted only for a tombstone,
// which has no envelope — that is the entire reason the header exists.
func (d *Delivery) ObservedAt() (time.Time, bool) {
	if !d.Tombstone && !d.Envelope.ObservedAt.IsZero() {
		return d.Envelope.ObservedAt, true
	}
	raw, ok := d.Headers[HeaderObservedAt]
	if !ok || raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || t.IsZero() {
		return time.Time{}, false
	}
	return t, true
}

// MarketID returns the record key as a domain.MarketID.
//
// It refuses when the topic is not keyed by market. That check is what keeps the
// phase-1 identifier guarantee alive on the READ side: internal/domain/ids.go
// made confusing a MarketID with an EventID a compile error, and this is the one
// place in the pipeline where an identifier arrives as an untyped string and
// could be given the wrong type back.
func (d *Delivery) MarketID() (domain.MarketID, error) {
	if err := d.requireKeyKind(KeyKindMarketID); err != nil {
		return "", err
	}
	id, err := domain.NewMarketID(d.Key)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	return id, nil
}

// EventID returns the record key as a domain.EventID.
func (d *Delivery) EventID() (domain.EventID, error) {
	if err := d.requireKeyKind(KeyKindEventID); err != nil {
		return "", err
	}
	id, err := domain.NewEventID(d.Key)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	return id, nil
}

// WagerID returns the record key as a domain.WagerID.
func (d *Delivery) WagerID() (domain.WagerID, error) {
	if err := d.requireKeyKind(KeyKindWagerID); err != nil {
		return "", err
	}
	id, err := domain.NewWagerID(d.Key)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}
	return id, nil
}

// requireKeyKind checks the topic's declared key kind against what the caller
// asked for. An unregistered topic (a test topic, or one a later phase adds)
// declares KeyKindUnknown and is allowed through: this package does not know
// what such a topic is keyed by, and refusing would make it unusable rather
// than safer.
func (d *Delivery) requireKeyKind(want KeyKind) error {
	t, known := LookupTopic(d.Topic)
	if !known || t.KeyKind() == KeyKindUnknown {
		return nil
	}
	if t.KeyKind() != want {
		return fmt.Errorf("%w: %s is keyed by %s, not %s",
			ErrWrongKeyKind, d.Topic, t.KeyKind(), want)
	}
	return nil
}

// LogValue implements slog.LogValuer so a Delivery can be logged whole without
// spilling the payload — which is provider data, and on wager.events is user
// data — into every log line.
func (d *Delivery) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("topic", d.Topic),
		slog.Int("partition", int(d.Partition)),
		slog.Int64("offset", d.Offset),
		slog.String("key", d.Key),
		slog.String("type", d.Envelope.Type),
		slog.Bool("tombstone", d.Tombstone),
		slog.Int("bytes", len(d.value)),
	)
}

// newDelivery converts a fetched record into a Delivery.
//
// # A null value is a tombstone, and an empty one is treated as the same thing
//
// Kafka distinguishes a null value from a zero-length value, and only null is
// what the log cleaner deletes on. In practice nothing distinguishes them
// downstream — this package never produces a zero-length value, and every other
// writer of these topics is also this package — so both are read as a deletion.
// The alternative, rejecting a zero-length value as malformed, would turn a
// harmless ambiguity into a stalled consumer.
//
// A tombstone that lacks HeaderTombstone is still a tombstone; it simply has no
// stated reason. That is the shape a tombstone written by kafka-console-producer
// or by a Flink job would have, and refusing it would mean the snapshot could
// not be rebuilt after any manual intervention.
func newDelivery(r *kgo.Record) (*Delivery, error) {
	d := &Delivery{
		Topic:     r.Topic,
		Partition: r.Partition,
		Offset:    r.Offset,
		Key:       string(r.Key),
		Timestamp: r.Timestamp,
		Headers:   flattenHeaders(r.Headers),
		value:     r.Value,
	}

	if len(r.Value) == 0 {
		d.Tombstone = true
		d.TombstoneReason = d.Headers[HeaderTombstoneReason]
		return d, nil
	}

	if err := json.Unmarshal(r.Value, &d.Envelope); err != nil {
		return d, fmt.Errorf("%w: decode envelope at %s/%d offset %d: %w",
			ErrMalformedEnvelope, r.Topic, r.Partition, r.Offset, err)
	}
	if err := d.Envelope.Validate(); err != nil {
		return d, fmt.Errorf("%s/%d offset %d: %w", r.Topic, r.Partition, r.Offset, err)
	}
	return d, nil
}

// flattenHeaders turns the header slice into a map, keeping the first
// occurrence of a repeated key.
func flattenHeaders(hs []kgo.RecordHeader) map[string]string {
	if len(hs) == 0 {
		return nil
	}
	m := make(map[string]string, len(hs))
	for _, h := range hs {
		if _, seen := m[h.Key]; seen {
			continue
		}
		m[h.Key] = string(h.Value)
	}
	return m
}

// decodeFailureReason maps a decode error onto a bounded metric label. Never the
// error text: a malformed payload can put arbitrary bytes in a json error
// message, and an unbounded label value is a cardinality bomb with an untrusted
// author.
func decodeFailureReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnsupportedEnvelope):
		return "unsupported_version"
	case errors.Is(err, ErrMalformedEnvelope):
		return "malformed"
	default:
		return "unknown"
	}
}
