package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/anpl1623/sharpline/internal/domain"
)

// The BROKER-FREE half of the produce side.
//
// Everything here is decided before a byte reaches the network: option
// validation, key construction, the record the encoder builds, and the three
// guards that stand between a caller and an accidental tombstone. The behaviour
// that needs a broker — acknowledgement, async promises, flush-on-close, the
// difference between the two durability postures — is proven against a real
// KRaft container in test/integration/kafka_test.go, because CLAUDE.md §10 is
// explicit that a mocked broker proves nothing about the bugs that matter.
//
// The division is not arbitrary. A test that needs a broker to check that
// `MaxBufferedRecords: -1` is refused is a slow test that proves a fast thing.

// validProducerOptions is the smallest ProducerOptions that passes validation.
// Cases below mutate a copy so each one states exactly one thing that is wrong.
func validProducerOptions() ProducerOptions {
	return ProducerOptions{ClientOptions: validOptions()}
}

// unreachableProducerOptions points at a port nothing listens on, with a retry
// budget small enough that exhausting it is a test rather than a wait.
//
// 127.0.0.1:1 refuses the connection immediately, which is the ECONNREFUSED
// branch of IsTransientClusterError — so the constructor RETRIES and then gives
// up, which is the path being covered. A hostname that does not resolve would
// take the DNS branch and a firewalled address would hang; a refused connection
// is the only one that is both instant and genuinely transient.
func unreachableProducerOptions() ProducerOptions {
	opts := validProducerOptions()
	opts.Brokers = []string{"127.0.0.1:1"}
	opts.ConnectAttempts = 2
	opts.ConnectBackoff = time.Millisecond
	opts.ConnectBackoffMax = 2 * time.Millisecond
	opts.ProbeTimeout = 250 * time.Millisecond
	return opts
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// TestProducerOptionsValidate covers the gate every producer constructor runs
// before it builds a client.
//
// The interesting property is the one that is NOT here: there is no durability
// knob to validate. acks, idempotence, linger and the delivery timeout are fixed
// by which constructor is called, so no test can ask for a weaker posture —
// which is the whole reason the split is two types rather than one type with a
// field.
func TestProducerOptionsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*ProducerOptions)
		wantErr error
	}{
		{name: "valid", mutate: func(*ProducerOptions) {}},
		{
			name:   "zero MaxBufferedRecords means the default",
			mutate: func(o *ProducerOptions) { o.MaxBufferedRecords = 0 },
		},
		{
			name:   "zero FlushTimeout means the default",
			mutate: func(o *ProducerOptions) { o.FlushTimeout = 0 },
		},
		{
			name:    "negative MaxBufferedRecords",
			mutate:  func(o *ProducerOptions) { o.MaxBufferedRecords = -1 },
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "negative FlushTimeout",
			mutate:  func(o *ProducerOptions) { o.FlushTimeout = -time.Second },
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "the embedded ClientOptions are validated too",
			mutate:  func(o *ProducerOptions) { o.Brokers = nil },
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "a nil logger is refused",
			mutate:  func(o *ProducerOptions) { o.Logger = nil },
			wantErr: ErrInvalidOptions,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validProducerOptions()
			tc.mutate(&opts)

			err := opts.validate()
			switch {
			case tc.wantErr == nil && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tc.wantErr != nil && !errors.Is(err, tc.wantErr):
				t.Fatalf("validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestProducerConstructorsRefuseBadOptionsBeforeAnyIO proves the failure arrives
// from validation and not from a dial: both constructors are called with an
// address that would take the full connect budget to fail on, and both must
// return immediately because the options never got that far.
func TestProducerConstructorsRefuseBadOptionsBeforeAnyIO(t *testing.T) {
	t.Parallel()

	opts := unreachableProducerOptions()
	opts.Service = "" // the one thing that is wrong

	start := time.Now()
	if _, err := NewOddsProducer(t.Context(), opts); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewOddsProducer = %v, want ErrInvalidOptions", err)
	}
	if _, err := NewAuditProducer(t.Context(), opts); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("NewAuditProducer = %v, want ErrInvalidOptions", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the constructors took %s; a rejected configuration must not reach the network", elapsed)
	}
}

// TestProducerConstructorsProbeTheClusterBeforeReturning covers the startup
// gate.
//
// ClientOptions.SkipStartupProbe documents why the gate is on by default: "a
// client that returns successfully and then cannot reach the cluster pushes the
// failure into the first produce, where it is attributed to whatever business
// operation happened to be first." So an unreachable cluster must fail the
// CONSTRUCTOR, with ErrUnavailable naming the cause.
func TestProducerConstructorsProbeTheClusterBeforeReturning(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		open func(context.Context, ProducerOptions) error
	}{
		{"odds", func(ctx context.Context, o ProducerOptions) error {
			p, err := NewOddsProducer(ctx, o)
			if p != nil {
				_ = p.Close()
			}
			return err
		}},
		{"audit", func(ctx context.Context, o ProducerOptions) error {
			p, err := NewAuditProducer(ctx, o)
			if p != nil {
				_ = p.Close()
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if err := tc.open(t.Context(), unreachableProducerOptions()); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("open against an unreachable cluster = %v, want ErrUnavailable", err)
			}
		})
	}
}

// TestSkipStartupProbeReturnsAClientAgainstADeadCluster is the other half: the
// escape hatch exists and it does what it says, so the gate above is a decision
// rather than an accident of the dial path.
func TestSkipStartupProbeReturnsAClientAgainstADeadCluster(t *testing.T) {
	t.Parallel()

	opts := unreachableProducerOptions()
	opts.SkipStartupProbe = true

	p, err := NewOddsProducer(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewOddsProducer with SkipStartupProbe = %v, want a client", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	// And the readiness check still tells the truth about it.
	if err := p.Check(t.Context()); err == nil {
		t.Error("Check() on a producer whose cluster is unreachable returned nil; " +
			"a probe that latched the constructor's answer would be worse than none")
	}
	if p.Name() != checkerName {
		t.Errorf("Name() = %q, want %q", p.Name(), checkerName)
	}
}

// -----------------------------------------------------------------------------
// Record keys
// -----------------------------------------------------------------------------

// TestRecordKeyConstructorsCarryTheirKind pins the pairing the whole write-side
// type-safety argument rests on: a key built from a domain.MarketID reports
// KeyKindMarketID and nothing else can produce that pairing.
func TestRecordKeyConstructorsCarryTheirKind(t *testing.T) {
	t.Parallel()

	if got := marketKey(domain.MarketID("mkt-1")); got.kind != KeyKindMarketID || got.id != "mkt-1" {
		t.Errorf("marketKey = %+v, want {market_id, mkt-1}", got)
	}
	if got := eventKey(domain.EventID("evt-1")); got.kind != KeyKindEventID || got.id != "evt-1" {
		t.Errorf("eventKey = %+v, want {event_id, evt-1}", got)
	}
	if got := wagerKey(domain.WagerID("wgr-1")); got.kind != KeyKindWagerID || got.id != "wgr-1" {
		t.Errorf("wagerKey = %+v, want {wager_id, wgr-1}", got)
	}
}

// TestRecordKeyValidate covers the guard that refuses a key which cannot choose
// a partition or identify a snapshot entry.
//
// The empty case is the one that matters. A null key is legal Kafka — it means
// round-robin — and on a compacted topic it is worse than useless, because the
// log cleaner cannot compact it and it accumulates for ever in a log whose whole
// purpose is to converge.
func TestRecordKeyValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		key     recordKey
		wantErr bool
		wantMsg string
	}{
		{name: "market key", key: marketKey("mkt-1")},
		{name: "event key", key: eventKey("evt-1")},
		{name: "wager key", key: wagerKey("wgr-1")},
		{
			name: "empty id", key: marketKey(""),
			wantErr: true, wantMsg: "cannot be compacted",
		},
		{
			name: "no kind", key: recordKey{kind: KeyKindUnknown, id: "mkt-1"},
			wantErr: true, wantMsg: "no key kind",
		},
		{
			name:    "over the domain id limit",
			key:     marketKey(domain.MarketID(strings.Repeat("m", domain.MaxIDLen+1))),
			wantErr: true, wantMsg: "limit is " + strconv.Itoa(domain.MaxIDLen),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := tc.key.validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("validate() = %v, want ErrInvalidKey", err)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("validate() = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// buildRecord
// -----------------------------------------------------------------------------

// testProducerShell is a producer with no client, for the paths that decide
// before any I/O.
//
// A nil kgo.Client is deliberate rather than convenient: every method exercised
// through this value returns before it would touch the client, so a change that
// moved the network call ahead of a guard would panic here instead of silently
// producing a mis-keyed or unacknowledged record.
func testProducerShell() *producer { return &producer{service: testProducer} }

// TestBuildRecordProducesTheWireShape covers the happy path: the key, the
// envelope and the descriptive headers a consumer reads.
func TestBuildRecordProducesTheWireShape(t *testing.T) {
	t.Parallel()

	p := testProducerShell()
	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 123456789, time.UTC)

	rec, err := p.buildRecord(OddsNormalized(), marketKey("mkt-1"), Message{
		Type:       "odds.normalized.v1",
		ID:         "msg-1",
		ObservedAt: observedAt,
		Payload:    samplePayload{MarketID: "mkt-1", BookID: "book-a", Decimal: 2.47},
	})
	if err != nil {
		t.Fatalf("buildRecord: %v", err)
	}

	if rec.Topic != TopicOddsNormalized {
		t.Errorf("Topic = %q, want %q", rec.Topic, TopicOddsNormalized)
	}
	if string(rec.Key) != "mkt-1" {
		t.Errorf("Key = %q, want %q", rec.Key, "mkt-1")
	}

	var env Envelope
	if err := json.Unmarshal(rec.Value, &env); err != nil {
		t.Fatalf("the record value is not an envelope: %v", err)
	}
	if err := env.Validate(); err != nil {
		t.Fatalf("the produced envelope does not validate: %v", err)
	}
	if env.Producer != testProducer {
		t.Errorf("Producer = %q, want %q; the producer name is the bus layer's business, not the caller's",
			env.Producer, testProducer)
	}
	if !env.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v", env.ObservedAt, observedAt)
	}

	headers := flattenHeaders(rec.Headers)
	for key, want := range map[string]string{
		HeaderEnvelopeVersion: strconv.Itoa(EnvelopeVersion),
		HeaderMessageType:     "odds.normalized.v1",
		HeaderProducer:        testProducer,
		HeaderMessageID:       "msg-1",
		HeaderObservedAt:      observedAt.Format(time.RFC3339Nano),
	} {
		if headers[key] != want {
			t.Errorf("header %q = %q, want %q", key, headers[key], want)
		}
	}
	if headers[HeaderTombstone] != "" {
		t.Error("a value record carries the tombstone header; only the Tombstone API may set it")
	}

	if got, want := recordBytes(rec), len(rec.Key)+len(rec.Value); got <= want {
		t.Errorf("recordBytes = %d, want more than the %d bytes of key+value (headers count too)", got, want)
	}
}

// TestBuildRecordGuards covers every refusal, in the order buildRecord applies
// them.
//
// The ordering is itself a decision the comment on buildRecord states: the
// closed check comes first so a use-after-close reads as such rather than as a
// confusing encoding error, and the key-kind check comes before encoding so a
// mis-keyed publish costs no marshal.
func TestBuildRecordGuards(t *testing.T) {
	t.Parallel()

	msg := Message{Type: "odds.normalized.v1", Payload: samplePayload{MarketID: "mkt-1"}}

	t.Run("a closed producer refuses first", func(t *testing.T) {
		t.Parallel()

		p := testProducerShell()
		p.closed.set()

		// Everything else about this call is ALSO wrong — a zero topic, an empty
		// key, no payload. ErrClosed is what must come back, because that is the
		// fact the caller needs.
		if _, err := p.buildRecord(Topic{}, marketKey(""), Message{}); !errors.Is(err, ErrClosed) {
			t.Fatalf("buildRecord on a closed producer = %v, want ErrClosed", err)
		}
	})

	t.Run("zero topic", func(t *testing.T) {
		t.Parallel()

		if _, err := testProducerShell().buildRecord(Topic{}, marketKey("mkt-1"), msg); !errors.Is(err, ErrInvalidTopic) {
			t.Fatalf("buildRecord with a zero Topic = %v, want ErrInvalidTopic", err)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		t.Parallel()

		if _, err := testProducerShell().buildRecord(OddsNormalized(), marketKey(""), msg); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("buildRecord with an empty key = %v, want ErrInvalidKey", err)
		}
	})

	// THIS is the check that cannot be reached through the exported API, and that
	// is exactly why it needs a test. PublishNormalized takes a domain.MarketID,
	// so passing an EventID does not compile; the runtime check exists so that a
	// FUTURE publish method wired to the wrong topic fails loudly on its first
	// call instead of quietly writing two snapshot entries for one market.
	t.Run("wrong key kind for the topic", func(t *testing.T) {
		t.Parallel()

		_, err := testProducerShell().buildRecord(OddsNormalized(), eventKey("evt-1"), msg)
		if !errors.Is(err, ErrWrongKeyKind) {
			t.Fatalf("buildRecord keyed by event on odds.normalized = %v, want ErrWrongKeyKind", err)
		}
		for _, want := range []string{TopicOddsNormalized, "market_id", "event_id"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		}
	})

	// A topic outside the registry declares KeyKindUnknown, and the check is
	// skipped rather than failing: this package cannot know what such a topic is
	// keyed by, and refusing would make it unusable rather than safer.
	t.Run("an unregistered topic is not key-checked", func(t *testing.T) {
		t.Parallel()

		if _, err := testProducerShell().buildRecord(externalTopic("some.other.topic"), eventKey("evt-1"), msg); err != nil {
			t.Fatalf("buildRecord on an unregistered topic = %v, want it to be allowed", err)
		}
	})

	// The guard that makes an accidental tombstone impossible: the ONLY path to a
	// null value is the Tombstone API.
	t.Run("an empty payload is not a tombstone", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name    string
			payload any
		}{
			{"nil payload", nil},
			{"empty raw message", json.RawMessage(nil)},
			{"a payload that marshals to null", json.RawMessage(`null`)},
		} {
			_, err := testProducerShell().buildRecord(OddsNormalized(), marketKey("mkt-1"),
				Message{Type: "odds.normalized.v1", Payload: tc.payload})
			if !errors.Is(err, ErrEmptyPayload) {
				t.Errorf("%s: buildRecord = %v, want ErrEmptyPayload", tc.name, err)
			}
		}
	})

	t.Run("an unmarshallable payload", func(t *testing.T) {
		t.Parallel()

		_, err := testProducerShell().buildRecord(OddsNormalized(), marketKey("mkt-1"),
			Message{Type: "odds.normalized.v1", Payload: make(chan int)})
		if !errors.Is(err, ErrInvalidMessage) {
			t.Fatalf("buildRecord with an unmarshallable payload = %v, want ErrInvalidMessage", err)
		}
	})
}

// -----------------------------------------------------------------------------
// Tombstones
// -----------------------------------------------------------------------------

// TestTombstoneGuards covers the three refusals that stand between a caller and
// a permanent deletion.
//
// Each one is a distinct way the operation is meaningless or destructive, and
// each is checked BEFORE the client is touched — which this test proves by using
// a producer with no client at all.
func TestTombstoneGuards(t *testing.T) {
	t.Parallel()

	valid := Tombstone{Reason: "event settled; market swept", Acknowledge: AcknowledgeDeletesKeyFromSnapshot}

	t.Run("a closed producer refuses first", func(t *testing.T) {
		t.Parallel()

		p := testProducerShell()
		p.closed.set()

		if err := p.tombstone(t.Context(), OddsNormalized(), marketKey("mkt-1"), valid); !errors.Is(err, ErrClosed) {
			t.Fatalf("tombstone on a closed producer = %v, want ErrClosed", err)
		}
	})

	// A DECLARED retention topic: a null value there provably deletes nothing and
	// only leaves a valueless record every consumer must learn to ignore.
	t.Run("a declared retention topic names its cleanup policy", func(t *testing.T) {
		t.Parallel()

		err := testProducerShell().tombstone(t.Context(), WagerEvents(), wagerKey("wgr-1"), valid)
		if !errors.Is(err, ErrNotCompacted) {
			t.Fatalf("tombstone on wager.events = %v, want ErrNotCompacted", err)
		}
		if !strings.Contains(err.Error(), "cleanup.policy=delete") {
			t.Errorf("error %q does not state the topic's declared cleanup policy", err)
		}
	})

	// A topic OUTSIDE the registry is a different situation and says so: its
	// cleanup policy is unknown rather than known-wrong.
	t.Run("an unregistered topic says the policy is unknown", func(t *testing.T) {
		t.Parallel()

		err := testProducerShell().tombstone(t.Context(), externalTopic("some.other.topic"), marketKey("mkt-1"), valid)
		if !errors.Is(err, ErrNotCompacted) {
			t.Fatalf("tombstone on an unregistered topic = %v, want ErrNotCompacted", err)
		}
		if !strings.Contains(err.Error(), "not a declared topic") {
			t.Errorf("error %q does not distinguish an unknown policy from a known-wrong one", err)
		}
	})

	t.Run("an invalid key", func(t *testing.T) {
		t.Parallel()

		if err := testProducerShell().tombstone(t.Context(), OddsNormalized(), marketKey(""), valid); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("tombstone with an empty key = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("the wrong key kind", func(t *testing.T) {
		t.Parallel()

		if err := testProducerShell().tombstone(t.Context(), OddsNormalized(), eventKey("evt-1"), valid); !errors.Is(err, ErrWrongKeyKind) {
			t.Fatalf("tombstone keyed by event on odds.normalized = %v, want ErrWrongKeyKind", err)
		}
	})

	// The ceremony. A zero-valued Tombstone{} must never delete a market, and
	// neither must one that is acknowledged but says nothing about why.
	t.Run("an unacknowledged tombstone", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name string
			ts   Tombstone
		}{
			{"the zero value", Tombstone{}},
			{"acknowledged with no reason", Tombstone{Acknowledge: AcknowledgeDeletesKeyFromSnapshot}},
			{"a reason with no acknowledgement", Tombstone{Reason: "swept"}},
			{"an over-long reason", Tombstone{
				Reason:      strings.Repeat("x", 513),
				Acknowledge: AcknowledgeDeletesKeyFromSnapshot,
			}},
		} {
			err := testProducerShell().tombstone(t.Context(), OddsNormalized(), marketKey("mkt-1"), tc.ts)
			if !errors.Is(err, ErrTombstoneNotAcknowledged) {
				t.Errorf("%s: tombstone = %v, want ErrTombstoneNotAcknowledged", tc.name, err)
			}
		}
	})
}

// -----------------------------------------------------------------------------
// The type split
// -----------------------------------------------------------------------------

// asyncOddsPublisher and marketTombstoner are the two capabilities the odds
// posture has and the audit posture deliberately does not. Declared here, in a
// test, because nothing in the production tree needs them as interfaces — they
// exist to make "AuditProducer has no async method and no tombstone method" an
// assertion rather than a comment.
type asyncOddsPublisher interface {
	PublishNormalizedAsync(context.Context, domain.MarketID, Message, func(error)) error
}

type marketTombstoner interface {
	TombstoneNormalized(context.Context, domain.MarketID, Tombstone) error
}

// TestTheTwoProducerTypesHaveDifferentSurfaces pins the compile-time half of the
// posture split.
//
// doc.go argues the split is two types rather than one type with a knob. That
// argument is only true if the types actually differ, and the difference is not
// a runtime flag anybody can read — it is which methods exist. A type assertion
// against an interface the audit posture must NOT satisfy is the only way to
// state that as a test.
func TestTheTwoProducerTypesHaveDifferentSurfaces(t *testing.T) {
	t.Parallel()

	var odds any = (*OddsProducer)(nil)
	var audit any = (*AuditProducer)(nil)

	if _, ok := odds.(asyncOddsPublisher); !ok {
		t.Error("OddsProducer has no async publish method; the low-latency posture needs one")
	}
	if _, ok := odds.(marketTombstoner); !ok {
		t.Error("OddsProducer cannot tombstone a market; nothing else can delete one from the snapshot")
	}

	// The two refusals, and the reason each is a refusal rather than an omission:
	// a fire-and-forget audit entry is an audit entry whose loss the caller
	// cannot detect, and wager.events is retention-based so a null value there
	// deletes nothing.
	if _, ok := audit.(asyncOddsPublisher); ok {
		t.Error("AuditProducer gained an async publish method; a settlement entry whose loss " +
			"the caller cannot detect defeats the purpose of the audit trail")
	}
	if _, ok := audit.(marketTombstoner); ok {
		t.Error("AuditProducer gained a tombstone method; wager.events is retention-based and " +
			"a null value there deletes nothing")
	}

	// Both are readiness checkers, because both answer the same question.
	if _, ok := odds.(checker); !ok {
		t.Error("OddsProducer does not satisfy the readiness checker shape")
	}
	if _, ok := audit.(checker); !ok {
		t.Error("AuditProducer does not satisfy the readiness checker shape")
	}
}

// TestProduceBatchHookFeedsTheBatchSizeHistogram covers the one metric that
// cannot be observed from a Produce call site.
//
// It exists to CHECK rather than assume doc.go's claim that linger=0 still
// batches: if a high produce rate did not show batches well above 1, the claim
// would be wrong and the linger setting would need revisiting. Here the hook is
// driven directly, because the value it records comes from franz-go and the only
// thing this package owns is that it reaches the right collector.
func TestProduceBatchHookFeedsTheBatchSizeHistogram(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	hook := &produceBatchHook{m: m}
	hook.OnProduceBatchWritten(kgo.BrokerMetadata{}, TopicOddsNormalized, 0, kgo.ProduceBatchMetrics{NumRecords: 37})

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "sharpline_kafka_produce_batch_records" {
			continue
		}
		for _, metric := range f.GetMetric() {
			if metric.GetHistogram().GetSampleCount() != 1 {
				t.Errorf("batch histogram observed %d samples, want 1", metric.GetHistogram().GetSampleCount())
			}
			if metric.GetHistogram().GetSampleSum() != 37 {
				t.Errorf("batch histogram sum = %v, want 37", metric.GetHistogram().GetSampleSum())
			}
		}
		return
	}
	t.Fatal("sharpline_kafka_produce_batch_records was not emitted; the hook is wired to nothing")
}
