package kafka

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The wire format is the one thing in this package that outlives the process
// that wrote it. A compacted topic keyed by market IS the current-line snapshot
// (CLAUDE.md §3), which means every record on odds.normalized may be decoded by a
// build that does not exist yet. These tests are therefore about two things:
// that a record survives a round trip byte-for-byte, and that a record this build
// cannot correctly read is REFUSED rather than half-decoded.

// testProducer is the service name the encoder stamps in these tests. It is a
// real service name from CLAUDE.md §3's table so the assertions read like the
// records the system actually writes.
const testProducer = "ingest"

// samplePayload is a payload shaped like a normalized odds message: a market
// identifier, a book, and a decimal price. It is created BY the test and asserted
// on by the test — the "no mock data" rule in the contract ledger forbids canned
// payloads standing in for ingested data, and this is neither shipped nor
// pre-published.
type samplePayload struct {
	MarketID string  `json:"market_id"`
	BookID   string  `json:"book_id"`
	Decimal  float64 `json:"decimal"`
}

// TestEnvelopeValidate covers the frame check every decoded record passes
// through.
//
// The version bounds are the load-bearing part. Reading a v2 record with a v1
// decoder does not fail — encoding/json happily ignores what it does not
// recognise — so the ONLY thing standing between a future field rename and a
// silently corrupted snapshot is this comparison.
func TestEnvelopeValidate(t *testing.T) {
	t.Parallel()

	valid := Envelope{
		Version:  EnvelopeVersion,
		Type:     "odds.normalized.v1",
		Producer: testProducer,
		Data:     json.RawMessage(`{"market_id":"m1"}`),
	}

	tests := []struct {
		name     string
		envelope Envelope
		wantErr  error
		why      string
	}{
		{name: "a well-formed envelope", envelope: valid},
		{
			name: "version zero",
			envelope: func() Envelope {
				e := valid
				e.Version = 0
				return e
			}(),
			wantErr: ErrUnsupportedEnvelope,
			why:     "a record with no version is a record written by something that is not this package",
		},
		{
			name: "a version from the future",
			envelope: func() Envelope {
				e := valid
				e.Version = EnvelopeVersion + 1
				return e
			}(),
			wantErr: ErrUnsupportedEnvelope,
			why:     "refusing is the entire point; reading it with an older decoder poisons a replayable log",
		},
		{
			name: "a version below the supported floor",
			envelope: func() Envelope {
				e := valid
				e.Version = MinSupportedEnvelopeVersion - 1
				return e
			}(),
			wantErr: ErrUnsupportedEnvelope,
			why:     "abandoned envelope versions must fail loudly",
		},
		{
			name: "no type",
			envelope: func() Envelope {
				e := valid
				e.Type = ""
				return e
			}(),
			wantErr: ErrMalformedEnvelope,
			why:     "the type is what a consumer switches on",
		},
		{
			name: "no data",
			envelope: func() Envelope {
				e := valid
				e.Data = nil
				return e
			}(),
			wantErr: ErrMalformedEnvelope,
			why:     "an envelope with no payload is not a tombstone — a tombstone has no envelope at all",
		},
		{
			name: "zero-length data",
			envelope: func() Envelope {
				e := valid
				e.Data = json.RawMessage{}
				return e
			}(),
			wantErr: ErrMalformedEnvelope,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.envelope.Validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("Validate() = %v, want ok", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = ok, want %v (%s)", tc.wantErr, tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Validate() = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

// TestMessageEncodeRejectsBadInput covers every way a Message can fail before any
// I/O happens.
//
// Failing here rather than at the broker is what keeps a bad publish attributable
// to the call site that made it, instead of to a produce error several
// milliseconds and one goroutine away.
func TestMessageEncodeRejectsBadInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		message Message
		wantErr error
		why     string
	}{
		{
			name:    "no type",
			message: Message{Payload: samplePayload{MarketID: "m1"}},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "a type over the length limit",
			message: Message{Type: strings.Repeat("t", maxMessageTypeLen+1), Payload: samplePayload{}},
			wantErr: ErrInvalidMessage,
			why:     "the type is a Prometheus label value and a span attribute, so it is bounded by construction",
		},
		{
			name:    "a type at exactly the length limit is accepted",
			message: Message{Type: strings.Repeat("t", maxMessageTypeLen), Payload: samplePayload{}},
		},
		{
			name:    "an uppercase type",
			message: Message{Type: "Odds.Normalized.V1", Payload: samplePayload{}},
			wantErr: ErrInvalidMessage,
			why:     "one spelling per label value; case variants would split a metric series in two",
		},
		{
			name:    "a type with a space",
			message: Message{Type: "odds normalized", Payload: samplePayload{}},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "a type with a colon",
			message: Message{Type: "odds:normalized", Payload: samplePayload{}},
			wantErr: ErrInvalidMessage,
		},
		{
			name:    "a nil payload",
			message: Message{Type: "odds.normalized.v1"},
			wantErr: ErrEmptyPayload,
			why:     "on a compacted topic an empty value is a TOMBSTONE, so it is never produced by accident",
		},
		{
			name:    "an empty json.RawMessage payload",
			message: Message{Type: "odds.normalized.v1", Payload: json.RawMessage{}},
			wantErr: ErrEmptyPayload,
		},
		{
			name:    "a payload that marshals to null",
			message: Message{Type: "odds.normalized.v1", Payload: (*samplePayload)(nil)},
			wantErr: ErrEmptyPayload,
			why: "a typed nil pointer is not == nil in an interface, so it slips past the nil check and " +
				"marshals to four bytes that would pass a length test and decode into nothing",
		},
		{
			name:    "a literal null json.RawMessage",
			message: Message{Type: "odds.normalized.v1", Payload: json.RawMessage("null")},
			wantErr: ErrEmptyPayload,
		},
		{
			name:    "a payload json cannot marshal",
			message: Message{Type: "odds.normalized.v1", Payload: make(chan int)},
			wantErr: ErrInvalidMessage,
		},
		{
			name: "an id over domain.MaxIDLen",
			message: Message{
				Type:    "odds.normalized.v1",
				ID:      strings.Repeat("i", domain.MaxIDLen+1),
				Payload: samplePayload{},
			},
			wantErr: ErrInvalidMessage,
			why:     "the id becomes a map key in a deduplicating consumer; unbounded means a malformed payload sizes the map",
		},
		{
			name: "an id at exactly domain.MaxIDLen is accepted",
			message: Message{
				Type:    "odds.normalized.v1",
				ID:      strings.Repeat("i", domain.MaxIDLen),
				Payload: samplePayload{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := tc.message.encode(testProducer, time.Now())
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("encode() = %v, want ok", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("encode() = ok, want %v (%s)", tc.wantErr, tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("encode() = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

// TestMessageEncodeRoundTrip checks that what the encoder writes is what a
// decoder reads, field by field, including the fields the CALLER does not get to
// set.
//
// Producer and ProducedAt are stamped by the bus layer on purpose: a caller that
// could set them could get them wrong, and ProducedAt is one half of the
// provider-attributable staleness measurement.
func TestMessageEncodeRoundTrip(t *testing.T) {
	t.Parallel()

	// A non-UTC observation instant, deliberately: The Odds API returns
	// timestamps with an offset, and the encoder must canonicalise rather than
	// preserve the offset — two build machines in two zones must not produce two
	// spellings of one instant.
	zone := time.FixedZone("UTC+2", 2*60*60)
	observedAt := time.Date(2026, 8, 17, 11, 15, 30, 123456789, zone)
	producedAt := time.Date(2026, 8, 17, 9, 15, 31, 987654321, time.UTC)

	payload := samplePayload{MarketID: "mkt-round-trip", BookID: "book-a", Decimal: 2.4700000000000002}

	msg := Message{
		Type:       "odds.normalized.v1",
		ID:         "msg-round-trip",
		ObservedAt: observedAt,
		Payload:    payload,
	}

	value, env, err := msg.encode(testProducer, producedAt)
	if err != nil {
		t.Fatalf("encode(): %v", err)
	}

	// The returned Envelope and the bytes must agree — they are two views of the
	// same record and the producer publishes one while logging the other.
	var decoded Envelope
	if err := json.Unmarshal(value, &decoded); err != nil {
		t.Fatalf("the encoded value is not decodable JSON: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("the encoder produced an envelope its own validator refuses: %v", err)
	}

	if decoded.Version != EnvelopeVersion {
		t.Errorf("Version = %d, want %d", decoded.Version, EnvelopeVersion)
	}
	if decoded.Type != msg.Type {
		t.Errorf("Type = %q, want %q", decoded.Type, msg.Type)
	}
	if decoded.ID != msg.ID {
		t.Errorf("ID = %q, want %q", decoded.ID, msg.ID)
	}
	if decoded.Producer != testProducer {
		t.Errorf("Producer = %q, want %q", decoded.Producer, testProducer)
	}
	if !decoded.ProducedAt.Equal(producedAt) {
		t.Errorf("ProducedAt = %v, want %v", decoded.ProducedAt, producedAt)
	}
	if !decoded.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v", decoded.ObservedAt, observedAt)
	}
	if got, want := decoded.ObservedAt.Location(), time.UTC; got != want {
		t.Errorf("ObservedAt location = %v, want %v; the encoder must canonicalise to UTC", got, want)
	}
	if env.Producer != decoded.Producer || env.Type != decoded.Type || !env.ObservedAt.Equal(decoded.ObservedAt) {
		t.Errorf("the returned Envelope %+v disagrees with the encoded bytes %+v", env, decoded)
	}

	// doc.go claims a float64 survives a JSON round trip exactly, because Go's
	// encoder uses strconv's shortest-round-trip formatting. That claim underpins
	// the choice of JSON for a system whose core type is a price, so it is checked
	// rather than believed.
	var gotPayload samplePayload
	if err := json.Unmarshal(decoded.Data, &gotPayload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if gotPayload != payload {
		t.Errorf("payload = %+v, want %+v", gotPayload, payload)
	}
	if gotPayload.Decimal != payload.Decimal {
		t.Errorf("float64 did not survive the round trip: %v != %v", gotPayload.Decimal, payload.Decimal)
	}
}

// TestMessageEncodeOmitsAZeroObservedAt checks that a message with no provider
// observation emits NO observed_at field at all.
//
// The alternative — "0001-01-01T00:00:00Z" — is indistinguishable in kafka-ui
// from a real-but-wrong timestamp, and it would make the staleness SLO compute a
// two-thousand-year lag for every wager event.
func TestMessageEncodeOmitsAZeroObservedAt(t *testing.T) {
	t.Parallel()

	value, env, err := Message{
		Type:    "wager.placed.v1",
		Payload: samplePayload{MarketID: "m1"},
	}.encode("api", time.Now())
	if err != nil {
		t.Fatalf("encode(): %v", err)
	}

	if strings.Contains(string(value), "observed_at") {
		t.Errorf("encoded value carries observed_at for a message with none: %s", value)
	}
	if strings.Contains(string(value), "0001-01-01") {
		t.Errorf("encoded value carries the zero time: %s", value)
	}
	if !env.ObservedAt.IsZero() {
		t.Errorf("Envelope.ObservedAt = %v, want the zero time", env.ObservedAt)
	}

	// An empty ID must be omitted for the same reason: "id":"" is a claim that a
	// deduplicating consumer would key on.
	if strings.Contains(string(value), `"id"`) {
		t.Errorf("encoded value carries an empty id: %s", value)
	}
}

// TestEnvelopeHeadersMirrorTheEnvelope checks the header set on a normal record.
//
// The duplication exists so that a TOMBSTONE — which has no body at all — can
// still be self-describing. Here it is checked on the non-tombstone side, where
// the invariant is simply that the headers do not disagree with the envelope.
func TestEnvelopeHeadersMirrorTheEnvelope(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 123456789, time.UTC)

	t.Run("a fully populated envelope", func(t *testing.T) {
		t.Parallel()

		env := Envelope{
			Version:    EnvelopeVersion,
			Type:       "odds.normalized.v1",
			ID:         "msg-1",
			Producer:   testProducer,
			ObservedAt: observedAt,
			Data:       json.RawMessage(`{}`),
		}
		got := flattenHeaders(env.headers())

		want := map[string]string{
			HeaderEnvelopeVersion: "1",
			HeaderMessageType:     "odds.normalized.v1",
			HeaderProducer:        testProducer,
			HeaderMessageID:       "msg-1",
			HeaderObservedAt:      observedAt.Format(time.RFC3339Nano),
		}
		for key, wantValue := range want {
			if got[key] != wantValue {
				t.Errorf("header %q = %q, want %q", key, got[key], wantValue)
			}
		}
		if got[HeaderTombstone] != "" {
			t.Errorf("a non-tombstone record carries %q", HeaderTombstone)
		}

		// The observed-at header must be parseable back to the same instant —
		// it is the only description a tombstone's observation carries.
		parsed, err := time.Parse(time.RFC3339Nano, got[HeaderObservedAt])
		if err != nil {
			t.Fatalf("the observed-at header does not parse: %v", err)
		}
		if !parsed.Equal(observedAt) {
			t.Errorf("observed-at header round trip = %v, want %v", parsed, observedAt)
		}
	})

	t.Run("optional headers are omitted rather than emitted empty", func(t *testing.T) {
		t.Parallel()

		env := Envelope{
			Version:  EnvelopeVersion,
			Type:     "wager.placed.v1",
			Producer: "api",
			Data:     json.RawMessage(`{}`),
		}
		got := flattenHeaders(env.headers())

		if _, present := got[HeaderMessageID]; present {
			t.Errorf("%s present for an envelope with no id", HeaderMessageID)
		}
		if _, present := got[HeaderObservedAt]; present {
			t.Errorf("%s present for an envelope with no observation instant", HeaderObservedAt)
		}
		if got[HeaderProducer] != "api" {
			t.Errorf("header %q = %q, want %q", HeaderProducer, got[HeaderProducer], "api")
		}
	})

	t.Run("the header slice leaves room for the tracing headers", func(t *testing.T) {
		t.Parallel()

		// The propagator APPENDS traceparent (and possibly tracestate) to this
		// slice. Under-allocating is not a correctness bug, but a reallocation
		// on every record on the hot path is exactly the kind of avoidable cost
		// the capacity hint exists to prevent — and the hint is only right if it
		// exceeds the number of headers actually written.
		env := Envelope{
			Version: EnvelopeVersion, Type: "t", ID: "i", Producer: "p",
			ObservedAt: observedAt, Data: json.RawMessage(`{}`),
		}
		h := env.headers()
		if cap(h)-len(h) < 2 {
			t.Errorf("headers() has %d spare slots (len %d, cap %d); the propagator needs 2",
				cap(h)-len(h), len(h), cap(h))
		}
	})
}

// TestTombstoneCannotBeBuiltByAccident is the test that guards the most
// destructive operation in this package.
//
// A tombstone on odds.normalized removes a market from the snapshot every client
// builds on connect, and once delete.retention.ms elapses the tombstone itself is
// collected — so the deletion stops being observable and becomes
// indistinguishable from "that market never existed". There is no undo. The zero
// value must therefore be rejected.
func TestTombstoneCannotBeBuiltByAccident(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		tombstone Tombstone
		wantErr   error
		why       string
	}{
		{
			name:      "the zero value",
			tombstone: Tombstone{},
			wantErr:   ErrTombstoneNotAcknowledged,
			why:       "a zero-valued struct must never be able to delete a market",
		},
		{
			name:      "a reason without the acknowledgement",
			tombstone: Tombstone{Reason: "market settled and swept"},
			wantErr:   ErrTombstoneNotAcknowledged,
			why:       "the acknowledgement constant is what makes every deletion site greppable",
		},
		{
			name:      "the acknowledgement without a reason",
			tombstone: Tombstone{Acknowledge: AcknowledgeDeletesKeyFromSnapshot},
			wantErr:   ErrTombstoneNotAcknowledged,
			why:       "an operator reading kafka-ui six months later needs to tell a sweep from a bug",
		},
		{
			name:      "a wrong acknowledgement value",
			tombstone: Tombstone{Reason: "r", Acknowledge: TombstoneAcknowledgement("yes")},
			wantErr:   ErrTombstoneNotAcknowledged,
			why:       "the type is single-valued; anything else is a caller working around the ceremony",
		},
		{
			name: "a reason over the length limit",
			tombstone: Tombstone{
				Reason:      strings.Repeat("r", 513),
				Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
			},
			wantErr: ErrTombstoneNotAcknowledged,
			why:     "the reason is a record header, and headers count against the broker's message size",
		},
		{
			name: "a reason at exactly the limit is accepted",
			tombstone: Tombstone{
				Reason:      strings.Repeat("r", 512),
				Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
			},
		},
		{
			name: "a properly acknowledged tombstone",
			tombstone: Tombstone{
				Reason:      "event settled; market swept by the admin console",
				Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.tombstone.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validate() = %v, want ok", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = ok, want %v (%s)", tc.wantErr, tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("validate() = %v, want it to wrap %v", err, tc.wantErr)
			}
		})
	}
}

// TestAcknowledgementConstantIsGreppable pins the literal.
//
// Its stated purpose is that `grep -r AcknowledgeDeletesKeyFromSnapshot`
// enumerates every place in the repository that can delete a market. Changing the
// VALUE would not break that, but changing it to something a developer might type
// by accident would defeat the ceremony, so the literal is asserted.
func TestAcknowledgementConstantIsGreppable(t *testing.T) {
	t.Parallel()

	if got, want := string(AcknowledgeDeletesKeyFromSnapshot), "deletes-key-from-snapshot"; got != want {
		t.Errorf("AcknowledgeDeletesKeyFromSnapshot = %q, want %q", got, want)
	}
	if AcknowledgeDeletesKeyFromSnapshot == "" {
		t.Error("the acknowledgement constant is the empty string, which is the zero value it exists to reject")
	}
}

// TestTombstoneHeadersAreItsOnlyDescription checks that a deletion carries its
// own provenance, since its value is null and can carry nothing.
func TestTombstoneHeadersAreItsOnlyDescription(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 0, time.UTC)

	t.Run("with an observation instant", func(t *testing.T) {
		t.Parallel()

		got := flattenHeaders(Tombstone{
			Reason:      "event settled",
			Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
			ObservedAt:  observedAt,
		}.headers("settle"))

		want := map[string]string{
			HeaderEnvelopeVersion: "1",
			HeaderProducer:        "settle",
			HeaderTombstone:       tombstoneHeaderValue,
			HeaderTombstoneReason: "event settled",
			HeaderObservedAt:      observedAt.Format(time.RFC3339Nano),
		}
		for key, wantValue := range want {
			if got[key] != wantValue {
				t.Errorf("header %q = %q, want %q", key, got[key], wantValue)
			}
		}

		// A tombstone has no envelope, so it must NOT claim a message type —
		// a consumer that switched on one would dispatch a deletion to a
		// payload handler.
		if _, present := got[HeaderMessageType]; present {
			t.Errorf("a tombstone carries %q; it has no payload to type", HeaderMessageType)
		}
	})

	t.Run("without an observation instant", func(t *testing.T) {
		t.Parallel()

		got := flattenHeaders(Tombstone{
			Reason:      "suspended by admin",
			Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
		}.headers("api"))

		if _, present := got[HeaderObservedAt]; present {
			t.Errorf("%s present on a tombstone with no observation instant", HeaderObservedAt)
		}
		if got[HeaderTombstone] != tombstoneHeaderValue {
			t.Errorf("header %q = %q, want %q", HeaderTombstone, got[HeaderTombstone], tombstoneHeaderValue)
		}
	})
}

// newTestRecord builds a kgo.Record the way a broker would hand one back.
func newTestRecord(topic, key string, value []byte, headers []kgo.RecordHeader) *kgo.Record {
	return &kgo.Record{
		Topic:     topic,
		Partition: 3,
		Offset:    4242,
		Key:       []byte(key),
		Value:     value,
		Headers:   headers,
		Timestamp: time.Date(2026, 8, 17, 9, 16, 0, 0, time.UTC),
	}
}

// TestNewDeliveryDecodesAValueRecord covers the normal consume path end to end:
// a Message encoded by the producer half, wrapped in a record, read back.
func TestNewDeliveryDecodesAValueRecord(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 500000000, time.UTC)
	payload := samplePayload{MarketID: "mkt-delivery", BookID: "book-b", Decimal: 1.91}

	value, env, err := Message{
		Type:       "odds.normalized.v1",
		ID:         "msg-delivery",
		ObservedAt: observedAt,
		Payload:    payload,
	}.encode(testProducer, time.Date(2026, 8, 17, 9, 15, 31, 0, time.UTC))
	if err != nil {
		t.Fatalf("encode(): %v", err)
	}

	rec := newTestRecord(TopicOddsNormalized, "mkt-delivery", value, env.headers())

	d, err := newDelivery(rec)
	if err != nil {
		t.Fatalf("newDelivery(): %v", err)
	}

	if d.Tombstone {
		t.Error("Tombstone = true for a record with a value")
	}
	if d.Topic != TopicOddsNormalized || d.Partition != 3 || d.Offset != 4242 {
		t.Errorf("coordinates = %s/%d offset %d, want %s/3 offset 4242",
			d.Topic, d.Partition, d.Offset, TopicOddsNormalized)
	}
	if d.Key != "mkt-delivery" {
		t.Errorf("Key = %q, want %q", d.Key, "mkt-delivery")
	}
	if !d.Timestamp.Equal(rec.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", d.Timestamp, rec.Timestamp)
	}
	if d.Envelope.Type != "odds.normalized.v1" {
		t.Errorf("Envelope.Type = %q, want %q", d.Envelope.Type, "odds.normalized.v1")
	}
	if string(d.Value()) != string(value) {
		t.Errorf("Value() = %q, want the record's own bytes", d.Value())
	}

	var got samplePayload
	if err := d.Unmarshal(&got); err != nil {
		t.Fatalf("Unmarshal(): %v", err)
	}
	if got != payload {
		t.Errorf("payload = %+v, want %+v", got, payload)
	}

	// The envelope is authoritative for observed-at on a record that has one.
	gotObserved, ok := d.ObservedAt()
	if !ok {
		t.Fatal("ObservedAt() reported no observation instant")
	}
	if !gotObserved.Equal(observedAt) {
		t.Errorf("ObservedAt() = %v, want %v", gotObserved, observedAt)
	}

	// LogValue must not spill the payload: on wager.events it is user data.
	logged := d.LogValue().String()
	if strings.Contains(logged, payload.BookID) {
		t.Errorf("LogValue() leaks payload contents: %s", logged)
	}
	if !strings.Contains(logged, "4242") {
		t.Errorf("LogValue() omits the offset, which is what a bug report is expressed in: %s", logged)
	}
}

// TestNewDeliveryTreatsANullValueAsATombstone covers the deletion path, and the
// deliberate decision to treat a zero-length value the same way.
func TestNewDeliveryTreatsANullValueAsATombstone(t *testing.T) {
	t.Parallel()

	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 0, time.UTC)
	tomb := Tombstone{
		Reason:      "event settled",
		Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
		ObservedAt:  observedAt,
	}

	tests := []struct {
		name  string
		value []byte
	}{
		{name: "a nil value, which is what the broker stores", value: nil},
		{
			// Kafka distinguishes null from zero-length and only null is what
			// the log cleaner deletes on. Downstream nothing distinguishes
			// them, and rejecting a zero-length value would turn a harmless
			// ambiguity into a stalled consumer.
			name:  "a zero-length value",
			value: []byte{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := newDelivery(newTestRecord(TopicOddsNormalized, "mkt-gone", tc.value, tomb.headers("settle")))
			if err != nil {
				t.Fatalf("newDelivery(): %v", err)
			}
			if !d.Tombstone {
				t.Fatal("Tombstone = false for a record with no value")
			}
			if d.TombstoneReason != "event settled" {
				t.Errorf("TombstoneReason = %q, want %q", d.TombstoneReason, "event settled")
			}
			if d.Value() != nil && len(d.Value()) != 0 {
				t.Errorf("Value() = %q, want nothing for a tombstone", d.Value())
			}
			// Envelope holds a json.RawMessage, so it is not comparable with ==;
			// the fields that would matter to a consumer are checked instead.
			if d.Envelope.Version != 0 || d.Envelope.Type != "" || len(d.Envelope.Data) != 0 {
				t.Errorf("Envelope = %+v, want the zero value for a tombstone", d.Envelope)
			}

			// The header is the ONLY place a tombstone's observation instant can
			// live. This is the entire reason the header exists.
			got, ok := d.ObservedAt()
			if !ok {
				t.Fatal("ObservedAt() reported none; the tombstone header carries one")
			}
			if !got.Equal(observedAt) {
				t.Errorf("ObservedAt() = %v, want %v", got, observedAt)
			}

			// Unmarshalling a deletion is a caller bug, and it must be loud
			// rather than yielding an empty struct.
			var payload samplePayload
			err = d.Unmarshal(&payload)
			if err == nil {
				t.Fatal("Unmarshal() on a tombstone = ok, want an error")
			}
			if !errors.Is(err, ErrMalformedEnvelope) {
				t.Errorf("Unmarshal() = %v, want it to wrap ErrMalformedEnvelope", err)
			}
		})
	}
}

// TestNewDeliveryAcceptsAForeignTombstone checks that a deletion written by
// something other than this package is still read as a deletion.
//
// That is the shape a tombstone from kafka-console-producer or from a phase-12
// Flink job would have. Refusing it would mean the snapshot could not be rebuilt
// after any manual intervention — a strictness that costs availability and buys
// nothing.
func TestNewDeliveryAcceptsAForeignTombstone(t *testing.T) {
	t.Parallel()

	d, err := newDelivery(newTestRecord(TopicOddsNormalized, "mkt-gone", nil, nil))
	if err != nil {
		t.Fatalf("newDelivery(): %v", err)
	}
	if !d.Tombstone {
		t.Fatal("Tombstone = false for a headerless null-valued record")
	}
	if d.TombstoneReason != "" {
		t.Errorf("TombstoneReason = %q, want empty for a tombstone this package did not write", d.TombstoneReason)
	}
	if d.Headers != nil {
		t.Errorf("Headers = %v, want nil for a record with none", d.Headers)
	}
	if _, ok := d.ObservedAt(); ok {
		t.Error("ObservedAt() reported an instant for a record carrying none")
	}
}

// TestNewDeliveryRefusesAnUndecodableRecord covers the two ways a record can be
// unreadable, and asserts that BOTH still return a Delivery.
//
// The partially populated Delivery is load-bearing: the consumer needs the
// topic, partition and offset to report which record poisoned the loop and to
// decide where to resume. An error with a nil Delivery would leave it with a
// metric increment and no coordinates.
func TestNewDeliveryRefusesAnUndecodableRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		value   []byte
		wantErr error
		why     string
	}{
		{
			name:    "not JSON at all",
			value:   []byte("{not json"),
			wantErr: ErrMalformedEnvelope,
		},
		{
			name:    "JSON that is not an envelope",
			value:   []byte(`["a","b"]`),
			wantErr: ErrMalformedEnvelope,
			why:     "an array does not unmarshal into the envelope struct",
		},
		{
			name:    "an envelope from a future build",
			value:   []byte(`{"v":2,"type":"odds.normalized.v2","producer":"ingest","produced_at":"2026-08-17T09:15:31Z","data":{}}`),
			wantErr: ErrUnsupportedEnvelope,
			why:     "this is the failure the version field exists to make loud",
		},
		{
			name:    "an envelope with no type",
			value:   []byte(`{"v":1,"producer":"ingest","produced_at":"2026-08-17T09:15:31Z","data":{}}`),
			wantErr: ErrMalformedEnvelope,
		},
		{
			name:    "an envelope with no data",
			value:   []byte(`{"v":1,"type":"odds.normalized.v1","producer":"ingest","produced_at":"2026-08-17T09:15:31Z"}`),
			wantErr: ErrMalformedEnvelope,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d, err := newDelivery(newTestRecord(TopicOddsNormalized, "mkt-bad", tc.value, nil))
			if err == nil {
				t.Fatalf("newDelivery() = ok, want %v (%s)", tc.wantErr, tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("newDelivery() = %v, want it to wrap %v", err, tc.wantErr)
			}
			if d == nil {
				t.Fatal("newDelivery() returned a nil Delivery with the error; the consumer needs the coordinates to report which record failed")
			}
			if d.Topic != TopicOddsNormalized || d.Partition != 3 || d.Offset != 4242 {
				t.Errorf("coordinates = %s/%d offset %d, want %s/3 offset 4242",
					d.Topic, d.Partition, d.Offset, TopicOddsNormalized)
			}

			// The error must name the record, because a poison record under
			// at-least-once retries for ever and the log line is the only way
			// to find it.
			if !strings.Contains(err.Error(), "4242") {
				t.Errorf("error %q does not name the offset", err)
			}
		})
	}
}

// TestDeliveryKeyExtractionIsTypeSafe is the read-side half of phase 1's
// identifier guarantee.
//
// internal/domain/ids.go made confusing a MarketID with an EventID a COMPILE
// error, and names the compacted Kafka key as one of the two reasons it exists.
// A consumed record is the one place in the pipeline where an identifier arrives
// as an untyped string, so it is the one place that guarantee can be lost — and
// the loss would be silent, producing a plausible identifier of the wrong sort.
func TestDeliveryKeyExtractionIsTypeSafe(t *testing.T) {
	t.Parallel()

	const key = "id-abc123"

	type extractor struct {
		name string
		call func(*Delivery) (string, error)
	}
	market := extractor{"MarketID", func(d *Delivery) (string, error) {
		id, err := d.MarketID()
		return id.String(), err
	}}
	event := extractor{"EventID", func(d *Delivery) (string, error) {
		id, err := d.EventID()
		return id.String(), err
	}}
	wager := extractor{"WagerID", func(d *Delivery) (string, error) {
		id, err := d.WagerID()
		return id.String(), err
	}}

	tests := []struct {
		name    string
		topic   string
		ex      extractor
		wantErr error
		why     string
	}{
		{name: "odds.normalized yields a MarketID", topic: TopicOddsNormalized, ex: market},
		{name: "price.computed yields a MarketID", topic: TopicPriceComputed, ex: market},
		{name: "wager.events yields a WagerID", topic: TopicWagerEvents, ex: wager},
		{name: "odds.raw.synthetic yields an EventID", topic: "odds.raw.synthetic", ex: event},

		{
			name: "odds.normalized refuses an EventID", topic: TopicOddsNormalized, ex: event,
			wantErr: ErrWrongKeyKind,
			why:     "a market id read as an event id routes a subscription to the wrong channel",
		},
		{
			name: "odds.normalized refuses a WagerID", topic: TopicOddsNormalized, ex: wager,
			wantErr: ErrWrongKeyKind,
		},
		{
			name: "price.computed refuses an EventID", topic: TopicPriceComputed, ex: event,
			wantErr: ErrWrongKeyKind,
		},
		{
			name: "wager.events refuses a MarketID", topic: TopicWagerEvents, ex: market,
			wantErr: ErrWrongKeyKind,
			why:     "the audit trail is keyed by wager; reading it as a market would silently mis-attribute a settlement",
		},
		{
			name: "odds.raw.synthetic refuses a MarketID", topic: "odds.raw.synthetic", ex: market,
			wantErr: ErrWrongKeyKind,
			why:     "the raw topic is keyed by event because one provider payload carries every market on a contest",
		},
		{
			// An unregistered topic declares KeyKindUnknown and is allowed
			// through: this package cannot know what a test topic or a phase-12
			// signals.* topic is keyed by, and refusing would make it unusable
			// rather than safer.
			name: "an unregistered topic permits any extraction", topic: "sharpline-it-throwaway", ex: market,
		},
		{
			name: "an unregistered topic permits an EventID too", topic: "sharpline-it-throwaway", ex: event,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := &Delivery{Topic: tc.topic, Key: key}
			got, err := tc.ex.call(d)

			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("%s() = %v, want ok", tc.ex.name, err)
				}
				if got != key {
					t.Errorf("%s() = %q, want %q", tc.ex.name, got, key)
				}
				return
			}

			if err == nil {
				t.Fatalf("%s() = %q, want %v (%s)", tc.ex.name, got, tc.wantErr, tc.why)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s() = %v, want it to wrap %v", tc.ex.name, err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("%s() = %q on the error path, want the zero identifier", tc.ex.name, got)
			}
		})
	}
}

// TestDeliveryKeyExtractionRejectsAMalformedKey checks that a key the domain's
// charset forbids fails even on the correctly-kinded topic.
//
// A key chooses the partition, and on a compacted topic it also identifies the
// snapshot entry, so a malformed key is never tolerated. The colon case is the
// specific one phase 1 called out: WebSocket channels are `market:{id}`, so an id
// containing a colon would make splitting a channel name ambiguous.
func TestDeliveryKeyExtractionRejectsAMalformedKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
		why  string
	}{
		{name: "empty", key: "", why: "a keyless record on a compacted topic can never be compacted"},
		{name: "a colon", key: "market:1", why: "would make market:{id} channel parsing ambiguous"},
		{name: "a space", key: "market 1", why: "whitespace in a log line and a metric"},
		{name: "a slash", key: "market/1"},
		{name: "over domain.MaxIDLen", key: strings.Repeat("k", domain.MaxIDLen+1)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := &Delivery{Topic: TopicOddsNormalized, Key: tc.key}
			got, err := d.MarketID()
			if err == nil {
				t.Fatalf("MarketID() = %q, want an error (%s)", got, tc.why)
			}
			if !errors.Is(err, ErrInvalidKey) {
				t.Errorf("MarketID() = %v, want it to wrap ErrInvalidKey", err)
			}
		})
	}
}

// TestDeliveryObservedAtPrefersTheEnvelope pins the precedence rule.
//
// The envelope is authoritative; the header is consulted only where the envelope
// is absent. If they ever disagree on a record that has both — which nothing this
// package writes can produce — the envelope wins, because a header is metadata a
// relay could rewrite and the envelope is the record itself.
func TestDeliveryObservedAtPrefersTheEnvelope(t *testing.T) {
	t.Parallel()

	envelopeTime := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	headerTime := time.Date(2026, 8, 17, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		delivery Delivery
		want     time.Time
		wantOK   bool
	}{
		{
			name: "the envelope wins when both are present",
			delivery: Delivery{
				Envelope: Envelope{ObservedAt: envelopeTime},
				Headers:  map[string]string{HeaderObservedAt: headerTime.Format(time.RFC3339Nano)},
			},
			want: envelopeTime, wantOK: true,
		},
		{
			name: "the header is used when the envelope has none",
			delivery: Delivery{
				Headers: map[string]string{HeaderObservedAt: headerTime.Format(time.RFC3339Nano)},
			},
			want: headerTime, wantOK: true,
		},
		{
			name: "a tombstone reads the header even against a populated envelope",
			delivery: Delivery{
				Tombstone: true,
				Envelope:  Envelope{ObservedAt: envelopeTime},
				Headers:   map[string]string{HeaderObservedAt: headerTime.Format(time.RFC3339Nano)},
			},
			want: headerTime, wantOK: true,
		},
		{
			name:     "neither present",
			delivery: Delivery{},
		},
		{
			name:     "an empty header value",
			delivery: Delivery{Headers: map[string]string{HeaderObservedAt: ""}},
		},
		{
			// A malformed header must report ABSENT, not zero. A zero instant
			// fed into the staleness SLO reports a two-thousand-year lag and
			// pages someone.
			name:     "an unparseable header value",
			delivery: Delivery{Headers: map[string]string{HeaderObservedAt: "17 August 2026"}},
		},
		{
			name:     "the zero time spelled out in the header",
			delivery: Delivery{Headers: map[string]string{HeaderObservedAt: "0001-01-01T00:00:00Z"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := tc.delivery.ObservedAt()
			if ok != tc.wantOK {
				t.Fatalf("ObservedAt() ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				if !got.IsZero() {
					t.Errorf("ObservedAt() = %v with ok=false, want the zero time", got)
				}
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("ObservedAt() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestFlattenHeaders covers the map conversion, including the repeated-key rule.
func TestFlattenHeaders(t *testing.T) {
	t.Parallel()

	t.Run("no headers yields nil rather than an empty map", func(t *testing.T) {
		t.Parallel()
		if got := flattenHeaders(nil); got != nil {
			t.Errorf("flattenHeaders(nil) = %v, want nil", got)
		}
		if got := flattenHeaders([]kgo.RecordHeader{}); got != nil {
			t.Errorf("flattenHeaders(empty) = %v, want nil", got)
		}
	})

	t.Run("a repeated key keeps its first occurrence", func(t *testing.T) {
		t.Parallel()
		// Nothing this package writes repeats a key — the propagator's Set
		// replaces rather than appends for exactly this reason — but a foreign
		// producer can, and the choice must be deterministic rather than
		// map-iteration-order dependent.
		got := flattenHeaders([]kgo.RecordHeader{
			{Key: "traceparent", Value: []byte("first")},
			{Key: "traceparent", Value: []byte("second")},
			{Key: HeaderProducer, Value: []byte("ingest")},
		})
		if got["traceparent"] != "first" {
			t.Errorf("traceparent = %q, want %q", got["traceparent"], "first")
		}
		if got[HeaderProducer] != "ingest" {
			t.Errorf("%s = %q, want %q", HeaderProducer, got[HeaderProducer], "ingest")
		}
		if len(got) != 2 {
			t.Errorf("len = %d, want 2", len(got))
		}
	})
}

// TestDecodeFailureReasonIsBounded checks the metric label mapping.
//
// The rule it enforces: a malformed payload can put arbitrary bytes in a json
// error message, and an unbounded Prometheus label value with an untrusted author
// is a cardinality bomb. So the label is drawn from a closed set and never from
// the error text.
func TestDecodeFailureReasonIsBounded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "no error", err: nil, want: ""},
		{name: "unsupported version", err: ErrUnsupportedEnvelope, want: "unsupported_version"},
		{
			name: "a wrapped unsupported version",
			err:  errors.New("x: " + ErrUnsupportedEnvelope.Error()),
			want: "unknown",
		},
		{name: "malformed", err: ErrMalformedEnvelope, want: "malformed"},
		{name: "anything else", err: errors.New("boom"), want: "unknown"},
		{
			// The specific hazard: an error carrying attacker-influenced bytes
			// must not reach the label.
			name: "an error whose text is a cardinality bomb",
			err:  errors.New(strings.Repeat("x", 4096)),
			want: "unknown",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := decodeFailureReason(tc.err); got != tc.want {
				t.Errorf("decodeFailureReason() = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("the reason set is closed", func(t *testing.T) {
		t.Parallel()
		allowed := map[string]bool{"": true, "unsupported_version": true, "malformed": true, "unknown": true}
		for _, err := range []error{
			nil,
			ErrUnsupportedEnvelope,
			ErrMalformedEnvelope,
			ErrInvalidKey,
			ErrEmptyPayload,
			ErrClosed,
			errors.New("arbitrary"),
		} {
			if got := decodeFailureReason(err); !allowed[got] {
				t.Errorf("decodeFailureReason(%v) = %q, which is outside the closed label set", err, got)
			}
		}
	})
}
