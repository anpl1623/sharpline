package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
)

// =============================================================================
// SCOPE OF THIS FILE, STATED PLAINLY
// =============================================================================
//
// These tests drive internal/platform/kafka — OddsProducer, AuditProducer,
// Consumer and Snapshotter — against a real KRaft broker at the same digest the
// compose stack runs.
//
// That is a correction, and the reason is worth recording. The first version of
// this file was written before producer.go and consumer.go existed. It drove raw
// kgo clients and hand-rolled its own envelope encoder, tombstone headers and
// trace carrier, each labelled at its definition as a stand-in. The assertions
// were good and they still are — they are preserved here almost verbatim — but
// they proved that KAFKA works, not that the wrapper the services will actually
// run works. The coverage report agreed: every function in producer.go and
// consumer.go was at zero while this file was green.
//
// So the mechanism changed and the claims did not. Where a test used to encode
// an envelope by hand it now calls PublishNormalized; where it used to poll a
// bare client and call CommitRecords it now runs a Consumer with a Handler. The
// exact counts, the no-loss and no-duplicate assertions, and the compaction and
// tombstone properties are the same assertions they were.
//
// =============================================================================

// itPayload is the payload shape these tests publish. It is created BY a test
// and asserted on by the same test; nothing here is a canned or pre-published
// record.
type itPayload struct {
	Market  string  `json:"market"`
	Book    string  `json:"book"`
	Decimal float64 `json:"decimal"`
	Version int     `json:"version"`
}

// itMessage builds the Message a test publishes. The envelope around it — the
// version, the producer name, the production instant — is the bus layer's
// business and is deliberately not reachable from here.
func itMessage(key string, version int, observedAt time.Time) kafka.Message {
	return kafka.Message{
		Type:       "odds.normalized.v1",
		ObservedAt: observedAt,
		Payload: itPayload{
			Market:  key,
			Book:    "book-a",
			Decimal: 1.5 + float64(version)/100,
			Version: version,
		},
	}
}

// publishNormalized publishes one version of one market and blocks for the ack.
// Synchronous on purpose: an assertion that runs before the ack is an assertion
// about the producer's buffer, not about the log.
func publishNormalized(t *testing.T, p *kafka.OddsProducer, key string, version int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := p.PublishNormalized(ctx, marketID(t, key), itMessage(key, version, time.Now().UTC())); err != nil {
		t.Fatalf("PublishNormalized(%s, v%d): %v", key, version, err)
	}
}

// -----------------------------------------------------------------------------
// Collecting what a Consumer delivered
// -----------------------------------------------------------------------------

// deliveryLog records what every member of a group handled, which is what makes
// "reprocessed" and "lost" separately observable.
type deliveryLog struct {
	mu       sync.Mutex
	byCoord  map[string]int
	byMember map[string]int
	keys     map[string]int
	payloads map[string]itPayload
}

func newDeliveryLog() *deliveryLog {
	return &deliveryLog{
		byCoord:  map[string]int{},
		byMember: map[string]int{},
		keys:     map[string]int{},
		payloads: map[string]itPayload{},
	}
}

// handler returns a Handler that records every delivery under a member's name.
//
// It satisfies the contract consumer.go states for a Handler: it returns
// promptly, it does not retain Delivery.Value() past its return, and it handles
// a tombstone.
func (l *deliveryLog) handler(t *testing.T, member string) kafka.Handler {
	return kafka.HandlerFunc(func(_ context.Context, d *kafka.Delivery) error {
		l.mu.Lock()
		defer l.mu.Unlock()

		l.byCoord[fmt.Sprintf("%d/%d", d.Partition, d.Offset)]++
		l.byMember[member]++
		l.keys[d.Key]++

		if d.Tombstone {
			delete(l.payloads, d.Key)
			return nil
		}
		var p itPayload
		if err := d.Unmarshal(&p); err != nil {
			t.Errorf("%s: unmarshal %s/%d offset %d: %v", member, d.Topic, d.Partition, d.Offset, err)
			return nil
		}
		l.payloads[d.Key] = p
		return nil
	})
}

func (l *deliveryLog) distinct() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.byCoord)
}

func (l *deliveryLog) coords() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.byCoord))
	for k, v := range l.byCoord {
		out[k] = v
	}
	return out
}

func (l *deliveryLog) memberTotals() map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]int, len(l.byMember))
	for k, v := range l.byMember {
		out[k] = v
	}
	return out
}

func (l *deliveryLog) keyCount(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.keys[key]
}

// =============================================================================
// 1. Round trip through the shipped producer and consumer
// =============================================================================

// TestPublishAndConsumeRoundTripsTheWholeRecord is the baseline: what a caller
// hands PublishRaw is what a Handler receives, including the metadata the bus
// layer adds on its behalf.
//
// It matters more than it sounds. The envelope is JSON, chosen over protobuf
// partly on the claim (doc.go) that "Go's encoding/json emits float64 with
// strconv's shortest-round-trip formatting, so a float64 survives a
// marshal/unmarshal cycle exactly" — for a system whose core type is a price,
// that claim is load-bearing and is checked here across a real broker rather than
// in memory.
//
// The read-side type safety is checked in the same pass, because it is only
// meaningful on a record that really came off a topic: odds.raw.* is keyed by
// EventID, so Delivery.EventID() must succeed and Delivery.MarketID() must
// refuse rather than hand back a plausible identifier of the wrong sort.
func TestPublishAndConsumeRoundTripsTheWholeRecord(t *testing.T) {
	t.Parallel()

	bus := newBusOptions(t)
	provider, topic := newRawTopic(t, 1)
	producer := newOddsProducer(t, bus)

	// A price with a mantissa that only survives shortest-round-trip formatting.
	payload := itPayload{Market: "mkt-1", Book: "book-a", Decimal: 2.4700000000000002, Version: 1}
	observedAt := time.Date(2026, 8, 17, 9, 15, 30, 123456789, time.UTC)
	event := eventID(t, uniqueID("evt"))

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	if err := producer.PublishRaw(ctx, provider, event, kafka.Message{
		Type:       "odds.raw.v1",
		ID:         "msg-round-trip",
		ObservedAt: observedAt,
		Payload:    payload,
	}); err != nil {
		t.Fatalf("PublishRaw: %v", err)
	}

	type received struct {
		delivery  kafka.Delivery
		headers   map[string]string
		payload   itPayload
		eventID   domain.EventID
		marketErr error
	}
	got := make(chan received, 1)

	startMember(t, bus, "reader", func(o *kafka.ConsumerOptions) {
		o.Group = uniqueID("sharpline-it-group")
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, kafka.HandlerFunc(func(_ context.Context, d *kafka.Delivery) error {
		id, err := d.EventID()
		if err != nil {
			t.Errorf("EventID() on a raw topic = %v, want the key back", err)
		}
		_, marketErr := d.MarketID()

		var p itPayload
		if err := d.Unmarshal(&p); err != nil {
			t.Errorf("Unmarshal: %v", err)
		}
		select {
		case got <- received{delivery: *d, headers: d.Headers, payload: p, eventID: id, marketErr: marketErr}:
		default:
		}
		return nil
	}))

	var r received
	select {
	case r = <-got:
	case <-time.After(90 * time.Second):
		t.Fatal("the consumer never delivered the published record")
	}

	if r.eventID != event {
		t.Errorf("EventID() = %q, want %q", r.eventID, event)
	}
	if !errors.Is(r.marketErr, kafka.ErrWrongKeyKind) {
		t.Errorf("MarketID() on an event-keyed topic = %v, want ErrWrongKeyKind; the phase-1 identifier "+
			"guarantee has to survive the one place an id arrives as an untyped string", r.marketErr)
	}
	if r.delivery.Topic != topic {
		t.Errorf("Topic = %q, want %q", r.delivery.Topic, topic)
	}

	if r.payload != payload {
		t.Errorf("payload = %+v, want %+v", r.payload, payload)
	}
	if r.payload.Decimal != payload.Decimal {
		t.Errorf("float64 did not survive the bus exactly: %v != %v", r.payload.Decimal, payload.Decimal)
	}

	env := r.delivery.Envelope
	if env.Version != kafka.EnvelopeVersion {
		t.Errorf("envelope Version = %d, want %d", env.Version, kafka.EnvelopeVersion)
	}
	if env.Type != "odds.raw.v1" || env.ID != "msg-round-trip" {
		t.Errorf("envelope identity changed across the bus: %+v", env)
	}
	if env.Producer != itService {
		t.Errorf("envelope Producer = %q, want %q — the producer name is stamped by the bus layer",
			env.Producer, itService)
	}
	if !env.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v — this is the staleness SLO's subtrahend and it is "+
			"propagated unchanged", env.ObservedAt, observedAt)
	}
	if at, ok := r.delivery.ObservedAt(); !ok || !at.Equal(observedAt) {
		t.Errorf("Delivery.ObservedAt() = %v (present=%v), want %v", at, ok, observedAt)
	}

	// The descriptive headers travel with the record so a tombstone — which has
	// no body at all — can still be self-describing.
	for key, want := range map[string]string{
		kafka.HeaderMessageType: "odds.raw.v1",
		kafka.HeaderProducer:    itService,
		kafka.HeaderMessageID:   "msg-round-trip",
		kafka.HeaderObservedAt:  observedAt.Format(time.RFC3339Nano),
	} {
		if r.headers[key] != want {
			t.Errorf("header %q = %q, want %q", key, r.headers[key], want)
		}
	}

	if n := busMetric(t, bus.registry, "sharpline_kafka_produce_records_total",
		map[string]string{"topic": topic, "outcome": "ok"}); n != 1 {
		t.Errorf("produce_records_total{outcome=ok} = %v, want 1", n)
	}
	if n := busMetric(t, bus.registry, "sharpline_kafka_consume_records_total",
		map[string]string{"topic": topic}); n < 1 {
		t.Errorf("consume_records_total = %v, want at least 1", n)
	}
}

// TestPublishingToAnUndeclaredTopicIsRefusedAndBounded proves the §9 posture end
// to end, through the shipped producer.
//
// "Topics created by Terraform, not by hand", and the broker runs with
// KAFKA_AUTO_CREATE_TOPICS_ENABLE=false "so a typo surfaces as
// UNKNOWN_TOPIC_OR_PARTITION instead of silently conjuring a 1-partition topic
// with the wrong cleanup policy". A silently conjured odds.normalized would have
// cleanup.policy=delete, and the current-line snapshot would simply not exist.
//
// The producer's own bound is what makes the failure arrive at all: without
// RecordDeliveryTimeout the record would sit in the buffer retrying a metadata
// request that will never succeed.
func TestPublishingToAnUndeclaredTopicIsRefusedAndBounded(t *testing.T) {
	t.Parallel()

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	// A valid provider slug whose topic was never created.
	provider, err := kafka.NewProvider(uniqueID("undeclared"))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	undeclared := kafka.TopicOddsRawPrefix + provider.String()

	// Generous relative to the odds posture's own 30s budget, so that whatever
	// fails is the PRODUCER's bound and not this deadline.
	ctx, cancel := context.WithTimeout(t.Context(), kafka.OddsRecordDeliveryTimeout+60*time.Second)
	defer cancel()

	start := time.Now()
	err = producer.PublishRaw(ctx, provider, eventID(t, uniqueID("evt")),
		kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: "mkt-1"}})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("producing to the undeclared topic %s succeeded; the broker auto-created it", undeclared)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the publish failed on the CALLER's deadline after %s; the odds posture is supposed to "+
			"give up on its own budget of %s", elapsed, kafka.OddsRecordDeliveryTimeout)
	}
	if !strings.Contains(err.Error(), undeclared) {
		t.Errorf("the error does not name the topic: %v", err)
	}
	t.Logf("the odds posture gave up after %s with: %v", elapsed.Round(time.Millisecond), err)

	// The topic still does not exist, which is the property that matters.
	adm := newKafkaAdmin(t)
	listCtx, listCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer listCancel()

	details, listErr := adm.ListTopics(listCtx, undeclared)
	if listErr != nil {
		t.Fatalf("list topics: %v", listErr)
	}
	if d, ok := details[undeclared]; ok && d.Err == nil {
		t.Errorf("topic %s exists after a failed produce; auto-creation is enabled", undeclared)
	}

	// And the failure is counted, which is what keeps the KafkaProduceErrors
	// alert honest about a producer failing on a topic that is not there.
	if n := busMetric(t, bus.registry, "sharpline_kafka_produce_records_total",
		map[string]string{"topic": undeclared, "outcome": "error"}); n != 1 {
		t.Errorf("produce_records_total{outcome=error} = %v, want 1", n)
	}
}

// =============================================================================
// 2. The two durability postures
// =============================================================================

// TestTheTwoPosturesDifferInWhatBoundsAFailingProduce observes the difference
// doc.go declares between the two producer types, rather than restating it.
//
// The odds posture bounds a record itself: "a lost odds update is SELF-HEALING —
// the topic is compacted and keyed by market, the next provider poll recomputes
// the same market, and the next publish restores the current line. Better a
// 30-second-old line than a producer buffer that grows until the process dies."
//
// The audit posture has no bound of its own, deliberately: "a lost wager event is
// recoverable by NOTHING … Retrying for ever with a synchronous Publish means the
// caller stays blocked and can refuse to commit the surrounding database
// transaction, which is the correct failure. The bound is the caller's context
// deadline, which is where it belongs."
//
// So: point both at a cluster that does not exist, give both a caller deadline
// far beyond the odds budget, and watch which one returns.
func TestTheTwoPosturesDifferInWhatBoundsAFailingProduce(t *testing.T) {
	t.Parallel()

	bus := newBusOptions(t)
	// A port nothing listens on. SkipStartupProbe is what makes the constructor
	// hand back a client anyway — this test is about the PRODUCE bound, not about
	// the startup gate, which has its own test.
	deadCluster := func(o *kafka.ProducerOptions) {
		o.Brokers = []string{"127.0.0.1:1"}
		o.SkipStartupProbe = true
	}

	odds := newOddsProducer(t, bus, deadCluster)
	audit := newAuditProducer(t, bus, deadCluster)

	callerBudget := kafka.OddsRecordDeliveryTimeout + 90*time.Second

	auditCtx, cancelAudit := context.WithTimeout(context.Background(), callerBudget)
	defer cancelAudit()

	auditDone := make(chan error, 1)
	go func() {
		auditDone <- audit.PublishWagerEvent(auditCtx, wagerID(t, uniqueID("wgr")),
			kafka.Message{Type: "wager.settled.v1", Payload: itPayload{Market: "mkt-1"}})
	}()

	oddsCtx, cancelOdds := context.WithTimeout(context.Background(), callerBudget)
	defer cancelOdds()

	start := time.Now()
	oddsErr := odds.PublishNormalized(oddsCtx, marketID(t, uniqueID("mkt")), itMessage("mkt", 1, time.Now()))
	oddsElapsed := time.Since(start)

	if oddsErr == nil {
		t.Fatal("PublishNormalized succeeded against a cluster that does not exist")
	}
	if errors.Is(oddsErr, context.DeadlineExceeded) || errors.Is(oddsErr, context.Canceled) {
		t.Fatalf("the odds publish failed on the CALLER's deadline after %s; the whole point of "+
			"RecordDeliveryTimeout is that it gives up first", oddsElapsed)
	}
	if oddsElapsed >= callerBudget {
		t.Fatalf("the odds publish took %s, which is the caller's whole budget", oddsElapsed)
	}
	t.Logf("odds posture gave up on its own after %s: %v", oddsElapsed.Round(time.Millisecond), oddsErr)

	// THE ASSERTION. At the instant the odds producer has already given up, the
	// audit producer is still trying, because nothing but the caller bounds it.
	select {
	case err := <-auditDone:
		t.Fatalf("the audit publish returned after %s with %v; it is supposed to retry until the "+
			"CALLER gives up, so that a settlement event is never abandoned while the caller could "+
			"still refuse to commit", oddsElapsed.Round(time.Millisecond), err)
	default:
	}

	// And when the caller does give up, that is what it reports.
	cancelAudit()
	select {
	case err := <-auditDone:
		if err == nil {
			t.Fatal("the audit publish succeeded against a cluster that does not exist")
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the audit publish failed with %v, want the caller's cancellation; the caller's "+
				"context is the only bound this posture has", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the audit publish did not return when its caller's context was cancelled")
	}
}

// =============================================================================
// 3. Asynchronous delivery
// =============================================================================

// oversizedPayload is larger than the topic-level max.message.bytes the async
// tests create their topic with, so the broker refuses it with a NON-RETRIABLE
// protocol error and the promise settles immediately.
//
// A non-retriable rejection is the point: it is a delivery failure that arrives
// in milliseconds rather than at the end of a retry budget, so the assertion is
// about how the failure is SURFACED rather than about how long it takes.
//
// The bytes are RANDOM, and that is not decoration. The producer compresses
// every batch with lz4 (doc.go: "JSON from one topic is highly repetitive, so
// lz4 recovers most of the size difference against protobuf") and the broker
// enforces max.message.bytes on the COMPRESSED batch — so a payload of 64 KiB of
// one repeated character sails through a 1 KiB limit, which is exactly what the
// first version of this test discovered. Incompressible bytes are the only ones
// whose size the broker actually sees.
func oversizedPayload() itPayload {
	const size = 32 * 1024
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	b := make([]byte, size)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return itPayload{Market: string(b), Book: "book-a", Decimal: 1.5, Version: 1}
}

// TestNoAsyncDeliveryFailureIsSilent is a claim in producer.go turned into a
// test.
//
// The claim: "the delivery outcome arrives later, and there are exactly two ways
// it is surfaced and no third: done is called with it when done is non-nil, and
// it is logged at ERROR level with the topic, key and message type when done is
// nil. Either way produce_records_total{outcome="error"} and
// produce_errors_total increment. There is no configuration in which a delivery
// failure is silent — that is the difference between an asynchronous producer
// and a lossy one."
//
// Both configurations are exercised, because the dangerous one is the second:
// a caller that passes nil is asking for fire-and-forget, and fire-and-forget is
// exactly where a dropped error would never be noticed.
func TestNoAsyncDeliveryFailureIsSilent(t *testing.T) {
	t.Parallel()

	// The broker refuses anything over this, whatever the producer's own batch
	// limit is.
	tiny := map[string]string{"max.message.bytes": "1024"}

	t.Run("with a callback the caller learns about it", func(t *testing.T) {
		t.Parallel()

		bus := newBusOptions(t)
		provider, topic := newRawTopic(t, 1, tiny)
		producer := newOddsProducer(t, bus)

		delivered := make(chan error, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()

		if err := producer.PublishRawAsync(ctx, provider, eventID(t, uniqueID("evt")),
			kafka.Message{Type: "odds.raw.v1", Payload: oversizedPayload()},
			func(err error) { delivered <- err },
		); err != nil {
			t.Fatalf("PublishRawAsync returned %v before buffering; the record was accepted, "+
				"so the failure belongs in the callback", err)
		}

		select {
		case err := <-delivered:
			if err == nil {
				t.Fatal("the callback reported success for a record the broker refused")
			}
			if !strings.Contains(err.Error(), topic) {
				t.Errorf("the callback's error does not name the topic: %v", err)
			}
			t.Logf("delivery failure reached the callback: %v", err)
		case <-time.After(60 * time.Second):
			t.Fatal("the callback was never called; a delivery failure that reaches nobody is a lost record")
		}

		if n := busMetric(t, bus.registry, "sharpline_kafka_produce_records_total",
			map[string]string{"topic": topic, "outcome": "error"}); n != 1 {
			t.Errorf("produce_records_total{outcome=error} = %v, want 1", n)
		}
	})

	t.Run("with no callback it is logged and counted", func(t *testing.T) {
		t.Parallel()

		bus := newBusOptions(t)
		provider, topic := newRawTopic(t, 1, tiny)
		producer := newOddsProducer(t, bus)

		event := eventID(t, uniqueID("evt"))
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()

		if err := producer.PublishRawAsync(ctx, provider, event,
			kafka.Message{Type: "odds.raw.v1", Payload: oversizedPayload()}, nil); err != nil {
			t.Fatalf("PublishRawAsync: %v", err)
		}

		// Flush is how a caller with no promise waits for the outcome: it blocks
		// until every buffered record has been acknowledged or failed.
		if err := producer.Flush(ctx); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		logged := bus.log.String()
		if !strings.Contains(logged, "kafka async produce failed") {
			t.Fatalf("nothing was logged about the failed delivery. THE FAILURE WAS SILENT.\nlog was:\n%s", logged)
		}
		for _, want := range []string{topic, event.String(), "odds.raw.v1", `"level":"ERROR"`} {
			if !strings.Contains(logged, want) {
				t.Errorf("the delivery-failure log line does not carry %q; without it an operator "+
					"cannot tell which record was lost", want)
			}
		}

		if n := busMetric(t, bus.registry, "sharpline_kafka_produce_records_total",
			map[string]string{"topic": topic, "outcome": "error"}); n != 1 {
			t.Errorf("produce_records_total{outcome=error} = %v, want 1", n)
		}
		if n := busMetric(t, bus.registry, "sharpline_kafka_produce_errors_total",
			map[string]string{"topic": topic}); n != 1 {
			t.Errorf("produce_errors_total = %v, want 1", n)
		}
	})
}

// TestCloseFlushesWhatWasAcceptedButNotYetWritten covers the ordering inside
// Close, and it does so by showing what the other order costs.
//
// producer.go: "kgo.Client.Close fails every still-buffered record. Closing
// without flushing first therefore DISCARDS whatever the process had accepted but
// not yet written — on wager.events that is a settlement entry that no poller
// will ever re-derive. So: refuse new publishes, drain, wait for the promises the
// drain released, and only then close."
//
// The first half is the shipped producer: accept a batch asynchronously, close
// immediately, and every record must be in the log. The second half is a RAW
// kgo client — the only place in this file where one produces anything — closed
// without a flush, to demonstrate that Close on its own really does discard the
// buffer. Its linger is set so that the buffer's contents at Close are
// deterministic rather than a race; the mechanism being shown is Close's, not
// linger's.
func TestCloseFlushesWhatWasAcceptedButNotYetWritten(t *testing.T) {
	t.Parallel()

	const records = 200

	t.Run("the shipped producer flushes on close", func(t *testing.T) {
		t.Parallel()

		bus := newBusOptions(t)
		provider, topic := newRawTopic(t, 1)

		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()

		p, err := kafka.NewOddsProducer(ctx, kafka.ProducerOptions{ClientOptions: bus.ClientOptions})
		if err != nil {
			t.Fatalf("NewOddsProducer: %v", err)
		}

		var failures int64
		var mu sync.Mutex
		for i := 0; i < records; i++ {
			if err := p.PublishRawAsync(ctx, provider, eventID(t, fmt.Sprintf("evt-%03d", i)),
				kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: "mkt", Version: i}},
				func(err error) {
					if err != nil {
						mu.Lock()
						failures++
						mu.Unlock()
					}
				}); err != nil {
				t.Fatalf("PublishRawAsync(%d): %v", i, err)
			}
		}

		// No Flush call. Close must do it.
		if err := p.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		mu.Lock()
		gotFailures := failures
		mu.Unlock()
		if gotFailures != 0 {
			t.Errorf("%d of %d accepted records failed on close; Close is supposed to drain the "+
				"buffer before it closes the client", gotFailures, records)
		}
		if got := topicRecordCount(t, topic); got != records {
			t.Fatalf("the log holds %d of the %d records the producer accepted; Close did not flush",
				got, records)
		}

		// And Close is idempotent, and a closed producer refuses new work rather
		// than accepting it into a buffer nothing will drain.
		if err := p.Close(); err != nil {
			t.Errorf("second Close: %v", err)
		}
		if err := p.PublishNormalized(ctx, marketID(t, uniqueID("mkt")), itMessage("mkt", 1, time.Now())); !errors.Is(err, kafka.ErrClosed) {
			t.Errorf("PublishNormalized on a closed producer = %v, want ErrClosed", err)
		}
	})

	t.Run("closing without flushing discards the buffer", func(t *testing.T) {
		t.Parallel()

		_, topic := newRawTopic(t, 1)

		// THE CONTROL ARM, and the only raw produce in this file. Linger holds the
		// batch so that "buffered at the moment of Close" is a fact rather than a
		// race; what is being demonstrated is what Close does to a buffer, which
		// is the sentence producer.go's Close is built around.
		cl := newKafkaClient(t, kgo.ProducerLinger(5*time.Second))
		for i := 0; i < records; i++ {
			cl.Produce(context.Background(), &kgo.Record{
				Topic: topic,
				Key:   []byte(fmt.Sprintf("evt-%03d", i)),
				Value: []byte(`{"v":1,"type":"t","producer":"p","produced_at":"2026-08-17T00:00:00Z","data":{}}`),
			}, nil)
		}
		cl.Close()

		if got := topicRecordCount(t, topic); got != 0 {
			t.Fatalf("the log holds %d records after a close with no flush; the control arm proves "+
				"nothing, so the flush in the shipped Close is untested", got)
		}
	})
}

// topicRecordCount sums the end offsets of every partition, which on a
// freshly-created topic is the number of records it holds.
func topicRecordCount(t *testing.T, topic string) int64 {
	t.Helper()

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	listed, err := adm.ListEndOffsets(ctx, topic)
	if err != nil {
		t.Fatalf("list end offsets for %s: %v", topic, err)
	}
	if err := listed.Error(); err != nil {
		t.Fatalf("list end offsets for %s: %v", topic, err)
	}

	var total int64
	for _, byPartition := range listed {
		for _, o := range byPartition {
			total += o.Offset
		}
	}
	return total
}

// =============================================================================
// 4. Trace context
// =============================================================================

// TestTraceContextSurvivesTheBus is the test CLAUDE.md §9 implicitly asks for,
// driven through the package's OWN injection and extraction.
//
// §9 wants "traces spanning ingest → pricer → stream so a single odds update can
// be followed end to end". That is achievable ONLY if the trace context crosses
// the bus in the record itself. A producer span with no matching consumer span is
// worse than no instrumentation: it looks like instrumentation, and the failure —
// a no-op propagator, a header stripped by a relay, a consumer that starts a root
// span — is invisible on a dashboard and only discovered when someone actually
// tries to follow an update and finds four disconnected traces.
//
// The earlier version of this test carried its own header carrier and called the
// propagator itself, which proved that OpenTelemetry works. This one hands a
// TracerProvider to the producer and the consumer and asserts on the spans THEY
// emit, so the thing under test is otel.go.
func TestTraceContextSurvivesTheBus(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	})

	bus := newBusOptions(t)
	bus.TracerProvider = tp
	// Propagator is deliberately left nil: the package must default to a concrete
	// W3C propagator rather than the OTel global, which is a no-op until some
	// entrypoint installs one — and a no-op propagator produces spans that merely
	// fail to join up, which is the failure this test exists to catch.

	provider, topic := newRawTopic(t, 1)
	producer := newOddsProducer(t, bus)

	// ---- produce inside a caller span ------------------------------------
	ctx, callerSpan := tp.Tracer("sharpline-it").Start(t.Context(), "ingest poll")
	callerSC := callerSpan.SpanContext()
	if !callerSC.IsValid() {
		t.Fatal("the caller span context is invalid; the test's tracer is not recording")
	}

	publishCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := producer.PublishRaw(publishCtx, provider, eventID(t, uniqueID("evt")),
		kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: "mkt-trace", Decimal: 1.91}}); err != nil {
		t.Fatalf("PublishRaw: %v", err)
	}
	callerSpan.End()

	// ---- consume, and look at the context the handler is given -------------
	handlerSC := make(chan trace.SpanContext, 1)
	startMember(t, bus, "reader", func(o *kafka.ConsumerOptions) {
		o.Group = uniqueID("sharpline-it-group")
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, kafka.HandlerFunc(func(hctx context.Context, _ *kafka.Delivery) error {
		select {
		case handlerSC <- trace.SpanContextFromContext(hctx):
		default:
		}
		return nil
	}))

	var consumed trace.SpanContext
	select {
	case consumed = <-handlerSC:
	case <-time.After(90 * time.Second):
		t.Fatal("the consumer never delivered the published record")
	}

	if !consumed.IsValid() {
		t.Fatal("the handler's context carries no span; the consumer is not instrumented")
	}
	if consumed.TraceID() != callerSC.TraceID() {
		t.Errorf("the handler runs in trace %s, the publisher in %s; they are two traces, not one",
			consumed.TraceID(), callerSC.TraceID())
	}
	if !consumed.IsSampled() {
		t.Error("the sampling decision did not cross the bus; a sampled producer trace with an " +
			"unsampled consumer half is a trace that ends mid-pipeline")
	}

	// ---- and the exported spans really are parent and child ---------------
	awaitTrue(t, 30*time.Second, "the producer and consumer spans are exported", func() bool {
		return len(exporter.GetSpans()) >= 3
	})

	var producerSpan, consumerSpan tracetest.SpanStub
	for _, s := range exporter.GetSpans() {
		switch s.Name {
		case topic + " publish":
			producerSpan = s
		case topic + " process":
			consumerSpan = s
		}
	}
	if producerSpan.Name == "" {
		t.Fatalf("no %q span was exported; the producer emitted nothing", topic+" publish")
	}
	if consumerSpan.Name == "" {
		t.Fatalf("no %q span was exported; the consumer emitted nothing", topic+" process")
	}

	if producerSpan.SpanKind != trace.SpanKindProducer {
		t.Errorf("the produce span's kind is %v, want producer", producerSpan.SpanKind)
	}
	if consumerSpan.SpanKind != trace.SpanKindConsumer {
		t.Errorf("the consume span's kind is %v, want consumer", consumerSpan.SpanKind)
	}
	if producerSpan.Parent.SpanID() != callerSC.SpanID() {
		t.Errorf("the produce span's parent is %s, want the caller span %s",
			producerSpan.Parent.SpanID(), callerSC.SpanID())
	}
	if consumerSpan.Parent.SpanID() != producerSpan.SpanContext.SpanID() {
		t.Errorf("the consume span's parent is %s, want the produce span %s; the context did not "+
			"cross the bus", consumerSpan.Parent.SpanID(), producerSpan.SpanContext.SpanID())
	}
	if !consumerSpan.Parent.IsRemote() {
		t.Error("the consume span's parent is not marked remote; it was not reconstructed from the record")
	}

	attrs := map[string]string{}
	for _, kv := range consumerSpan.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["messaging.system"] != "kafka" || attrs["messaging.destination.name"] != topic {
		t.Errorf("the consume span's messaging attributes are %v", attrs)
	}
}

// =============================================================================
// 5. Compaction
// =============================================================================

// TestTerraformCompactionConfigReachesTheBroker checks that the catalogue's
// compaction settings survive to the broker AND that they are internally coherent.
//
// CLAUDE.md §3's central claim is that "a compacted topic keyed by market_id IS
// the current-line snapshot". That sentence is false unless the log cleaner
// actually runs, and Kafka's defaults at this scale mean it never does: the
// cleaner skips the ACTIVE segment, and a default 7-day segment.ms with a 1 GiB
// segment.bytes gives a low-volume topic exactly one segment which is always
// active. Terraform's module encodes that reasoning as plan-time preconditions;
// this asserts the resulting values are what the running broker holds.
//
// It runs against price.computed, which this suite creates with the catalogue's
// values VERBATIM. odds.normalized is created with three timing knobs
// accelerated so that compaction can be waited out inside a test — see
// compactionSpeedups — so it is not the topic to check the catalogue against.
// The two declarations are compared directly instead, which is a claim the
// catalogue itself makes ("Compaction settings are IDENTICAL to odds.normalized
// on purpose") and which nothing was checking.
func TestTerraformCompactionConfigReachesTheBroker(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	declared := terraformTopicConfig(t, kafka.TopicPriceComputed)
	normalized := terraformTopicConfig(t, kafka.TopicOddsNormalized)

	compactionKeys := []string{
		"segment.ms", "segment.bytes", "min.cleanable.dirty.ratio",
		"min.compaction.lag.ms", "max.compaction.lag.ms", "delete.retention.ms",
	}
	for _, key := range compactionKeys {
		if declared[key] != normalized[key] {
			t.Errorf("%s declares %s=%q and %s declares %q; the catalogue says they are identical "+
				"on purpose — it is the same snapshot property over the same key space",
				kafka.TopicPriceComputed, key, declared[key], kafka.TopicOddsNormalized, normalized[key])
		}
	}

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	resourceConfigs, err := adm.DescribeTopicConfigs(ctx, kafka.TopicPriceComputed)
	if err != nil {
		t.Fatalf("describe topic configs: %v", err)
	}
	rc, err := resourceConfigs.On(kafka.TopicPriceComputed, nil)
	if err != nil {
		t.Fatalf("describe topic configs for %s: %v", kafka.TopicPriceComputed, err)
	}

	onBroker := map[string]string{}
	for _, c := range rc.Configs {
		if c.Value != nil {
			onBroker[c.Key] = *c.Value
		}
	}

	// Every value the catalogue declares must be the value the broker holds.
	for key, want := range declared {
		got, ok := onBroker[key]
		if !ok {
			t.Errorf("the broker reports no value for %q, which Terraform declares as %q", key, want)
			continue
		}
		if got != want {
			t.Errorf("broker %q = %q, Terraform declares %q", key, got, want)
		}
	}
	if onBroker["cleanup.policy"] != "compact" {
		t.Errorf("cleanup.policy = %q, want compact", onBroker["cleanup.policy"])
	}

	// The coherence properties, restated as assertions rather than as prose.
	// Each is a way the snapshot claim silently stops being true.
	segmentMS := mustInt(t, declared, "segment.ms")
	maxLagMS := mustInt(t, declared, "max.compaction.lag.ms")
	minLagMS := mustInt(t, declared, "min.compaction.lag.ms")
	deleteRetentionMS := mustInt(t, declared, "delete.retention.ms")
	dirtyRatio := mustFloat(t, declared, "min.cleanable.dirty.ratio")

	if segmentMS <= 0 || segmentMS >= 7*24*3600*1000 {
		t.Errorf("segment.ms = %d; at or above Kafka's 7-day default a low-volume topic has one "+
			"always-active segment and the cleaner never runs", segmentMS)
	}
	if maxLagMS <= 0 {
		t.Errorf("max.compaction.lag.ms = %d; without it, cleaning is throughput-driven and a quiet "+
			"overnight slate may never reach the dirty ratio at all", maxLagMS)
	}
	if segmentMS > maxLagMS {
		t.Errorf("segment.ms (%d) exceeds max.compaction.lag.ms (%d); a record cannot be cleaned before "+
			"its segment closes, so the forced-compaction deadline cannot be met", segmentMS, maxLagMS)
	}
	if minLagMS >= maxLagMS {
		t.Errorf("min.compaction.lag.ms (%d) is not below max.compaction.lag.ms (%d)", minLagMS, maxLagMS)
	}
	if minLagMS <= 0 {
		t.Errorf("min.compaction.lag.ms = %d; at zero a superseded record can vanish the instant its "+
			"segment closes, which silently skips the intermediate line movements steam detection and "+
			"CLV are computed from", minLagMS)
	}
	if dirtyRatio <= 0 || dirtyRatio > 1 {
		t.Errorf("min.cleanable.dirty.ratio = %v, want (0, 1]; exactly 0 makes the cleaner re-scan an "+
			"already-clean log continuously", dirtyRatio)
	}
	if deleteRetentionMS < 24*3600*1000 {
		t.Errorf("delete.retention.ms = %d; a consumer that was down over a weekend would never see a "+
			"tombstone and would resurrect a suspended market's stale line", deleteRetentionMS)
	}
}

// snapshotKeyCounts reads a compacted topic through the SHIPPED Snapshotter and
// tallies how many records each of the given keys still has, splitting values
// from tombstones.
//
// Read rather than Snapshot, because the whole point is the record COUNT: the
// fold would collapse the versions this is trying to count.
func snapshotKeyCounts(t *testing.T, s *kafka.Snapshotter, keys map[string]bool) (values, tombstones map[string]int, stats kafka.SnapshotStats) {
	t.Helper()

	values = map[string]int{}
	tombstones = map[string]int{}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	stats, err := s.Read(ctx, func(_ context.Context, d *kafka.Delivery) error {
		if !keys[d.Key] {
			return nil
		}
		if d.Tombstone {
			tombstones[d.Key]++
		} else {
			values[d.Key]++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Snapshotter.Read: %v", err)
	}
	return values, tombstones, stats
}

// TestUncompactedTailStillNeedsTheFold documents WHY the snapshot reader folds
// last-write-wins instead of trusting compaction.
//
// With the catalogue's real values — a 1-hour segment roll and a 1-minute
// minimum compaction lag — the cleaner provably has not run by the time a test
// finishes, so a from-the-start read returns EVERY version of every key. Code
// that assumed one record per key on a compacted topic would be reading a stale
// value here and would be correct only after the cleaner eventually caught up.
//
// price.computed is the topic for this because it carries the catalogue's values
// verbatim.
func TestUncompactedTailStillNeedsTheFold(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	const versions = 4
	keys := []string{uniqueID("mkt-a"), uniqueID("mkt-b"), uniqueID("mkt-c")}
	mine := map[string]bool{}
	for _, k := range keys {
		mine[k] = true
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for v := 1; v <= versions; v++ {
		for _, key := range keys {
			if err := producer.PublishPrice(ctx, marketID(t, key), itMessage(key, v, time.Now().UTC())); err != nil {
				t.Fatalf("PublishPrice(%s, v%d): %v", key, v, err)
			}
		}
	}

	s := newSnapshotter(t, bus, kafka.TopicPriceComputed)
	values, tombstones, stats := snapshotKeyCounts(t, s, mine)

	for _, key := range keys {
		if values[key] != versions {
			t.Errorf("key %s has %d records in the log, want all %d versions; the cleaner is not "+
				"expected to have run yet with segment.ms=1h and min.compaction.lag.ms=60s",
				key, values[key], versions)
		}
		if tombstones[key] != 0 {
			t.Errorf("key %s has %d tombstones and this test wrote none", key, tombstones[key])
		}
	}
	if stats.Partitions < 1 {
		t.Errorf("the snapshot reported %d partitions", stats.Partitions)
	}

	// And the fold, which is what a consumer actually builds state from, resolves
	// to the latest version per key regardless.
	snapCtx, snapCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer snapCancel()

	state, _, err := s.Snapshot(snapCtx)
	if err != nil {
		t.Fatalf("Snapshotter.Snapshot: %v", err)
	}
	for _, key := range keys {
		raw, ok := state[key]
		if !ok {
			t.Errorf("the fold dropped key %s", key)
			continue
		}
		var p itPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode the folded payload for %s: %v", key, err)
		}
		if p.Version != versions {
			t.Errorf("key %s folded to version %d, want the latest %d", key, p.Version, versions)
		}
	}
}

// TestCompactionCollapsesToTheLatestValuePerKey proves the mechanism itself: the
// broker's log cleaner really does reduce the log to one record per key.
//
// # What is scaled, what is not
//
// odds.normalized is created on this broker with three timing knobs overridden
// and NOTHING else — see compactionSpeedups, which states each one and why the
// production value cannot be waited out. min.cleanable.dirty.ratio keeps the
// catalogue's 0.1 on purpose: it is not a blocker at this scale and leaving it
// proves so.
//
// The assertions are scoped to the keys this test wrote, because the topic is
// shared with every other test that publishes a market. That is stronger than
// the log-size comparison it replaces, not weaker: "each of MY keys collapsed to
// exactly one record, and that record is the latest version" is the property
// CLAUDE.md §3 actually depends on, while "the log got smaller" is a proxy for it.
//
// If this test fails, compaction does not work and CLAUDE.md §3's snapshot claim
// is false — which is a finding about the design, not about the test.
func TestCompactionCollapsesToTheLatestValuePerKey(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	const versions = 5
	keys := []string{uniqueID("mkt-a"), uniqueID("mkt-b"), uniqueID("mkt-c")}
	mine := map[string]bool{}
	for _, k := range keys {
		mine[k] = true
	}

	for v := 1; v <= versions; v++ {
		for _, key := range keys {
			publishNormalized(t, producer, key, v)
		}
	}

	s := newSnapshotter(t, bus, kafka.TopicOddsNormalized)

	// The precondition is that the log holds MORE THAN ONE record per key, not
	// that it still holds all `versions` of them.
	//
	// This is the only thing the assertion below needs: "exactly 1 afterwards"
	// is a demonstrated collapse precisely when there was more than 1 to
	// collapse, and it is vacuous otherwise. Demanding the exact count demands
	// something else entirely — that the cleaner has NOT RUN YET — and this
	// topic is deliberately configured so that it may run at any moment:
	// compactionSpeedups sets min.compaction.lag.ms to 0 and max.compaction.lag.ms
	// to 1s, so a record is cleanable almost as soon as its segment closes.
	//
	// Whether a segment closes between the publishes above and this read is
	// decided by the OTHER tests sharing odds.normalized — the cleaner never
	// touches the active segment, so it takes someone else's append to roll one
	// — and that traffic is not this test's to control. It failed exactly that
	// way under `go test -race` once the phase-6 gateway fixture began driving
	// the real normalizer onto this topic: one key read back 4 of its 5 records,
	// which is compaction working, reported as compaction broken.
	//
	// Nothing is lost by relaxing it. The claim that every publish landed is
	// still proven, and proven harder, at the END of this test: the one
	// surviving record per key is decoded and must be version `versions`. A
	// publish that never arrived cannot produce that.
	before, _, _ := snapshotKeyCounts(t, s, mine)
	for _, key := range keys {
		if before[key] < 2 {
			t.Fatalf("before the forced roll key %s holds %d record(s); with fewer than 2 there is "+
				"nothing for the cleaner to collapse and the assertion below would pass vacuously",
				key, before[key])
		}
	}

	// The cleaner never touches the ACTIVE segment, so something has to close it.
	// Waiting out segment.ms is not enough on its own: Kafka rolls a segment on
	// APPEND, when it notices the current one is too old. Hence the sleep and
	// then one more record — under a key of its own so it does not disturb the
	// counts being asserted. odds.normalized has ONE partition on this broker
	// precisely so that this roll reaches the log the keys above are in.
	time.Sleep(1500 * time.Millisecond)
	rollKey := uniqueID("mkt-roll")
	publishNormalized(t, producer, rollKey, 1)

	// Poll until the log converges. The cleaner is asynchronous by nature, so
	// this waits for an outcome rather than assuming a duration.
	deadline := time.Now().Add(90 * time.Second)
	var after map[string]int
	for time.Now().Before(deadline) {
		after, _, _ = snapshotKeyCounts(t, s, mine)
		collapsed := true
		for _, key := range keys {
			if after[key] != 1 {
				collapsed = false
			}
		}
		if collapsed {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	for _, key := range keys {
		if after[key] != 1 {
			t.Fatalf("key %s still holds %d records after %s (it held %d before). "+
				"COMPACTION DID NOT RUN. CLAUDE.md §3's claim that a compacted topic IS the "+
				"current-line snapshot does not hold with this configuration.",
				key, after[key], 90*time.Second, before[key])
		}
	}

	// The surviving record for each key is the LATEST one, which is the half of
	// the claim that makes the log a snapshot rather than merely a smaller log.
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	state, stats, err := s.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshotter.Snapshot: %v", err)
	}
	for _, key := range keys {
		raw, ok := state[key]
		if !ok {
			t.Errorf("key %s is missing after compaction; the cleaner must keep the latest value per key", key)
			continue
		}
		var p itPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode the folded payload for %s: %v", key, err)
		}
		if p.Version != versions {
			t.Errorf("key %s survived at version %d, want the latest %d", key, p.Version, versions)
		}
	}

	t.Logf("compaction reduced %d records per key to 1; the whole topic now reports %d values and %d tombstones",
		versions, stats.Values, stats.Tombstones)
}

// =============================================================================
// 6. Tombstones
// =============================================================================

// TestTombstoneRemovesAKeyFromTheSnapshot is the destructive half of the
// compacted-topic contract, driven through TombstoneNormalized.
//
// Three assertions, and they are different claims. The API must refuse an
// unacknowledged deletion, because a zero-valued Tombstone{} that could delete a
// market is a market deleted by accident. The FOLD must drop the key
// immediately, because a consumer that ignores a tombstone leaves a deleted
// market in its cache for ever — no further record for that key is coming. And
// the CLEANER must eventually drop the key's superseded values from the log,
// which is what makes the deletion survive a replay from scratch.
func TestTombstoneRemovesAKeyFromTheSnapshot(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	kept := uniqueID("mkt-kept")
	doomed := uniqueID("mkt-doomed")
	mine := map[string]bool{kept: true, doomed: true}

	publishNormalized(t, producer, kept, 1)
	publishNormalized(t, producer, doomed, 1)
	publishNormalized(t, producer, kept, 2)
	publishNormalized(t, producer, doomed, 2)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// The ceremony is not optional, and it is checked on the real path rather
	// than only in a unit test: this is the call that actually deletes a market.
	for _, bad := range []kafka.Tombstone{
		{},
		{Reason: "no acknowledgement"},
		{Acknowledge: kafka.AcknowledgeDeletesKeyFromSnapshot},
	} {
		if err := producer.TombstoneNormalized(ctx, marketID(t, doomed), bad); !errors.Is(err, kafka.ErrTombstoneNotAcknowledged) {
			t.Fatalf("TombstoneNormalized(%+v) = %v, want ErrTombstoneNotAcknowledged", bad, err)
		}
	}

	const reason = "event settled; market swept by the admin console"
	if err := producer.TombstoneNormalized(ctx, marketID(t, doomed), kafka.Tombstone{
		Reason:      reason,
		Acknowledge: kafka.AcknowledgeDeletesKeyFromSnapshot,
	}); err != nil {
		t.Fatalf("TombstoneNormalized: %v", err)
	}

	// A tombstone on a retention topic deletes nothing, and the type system does
	// not stop a caller reaching for one — the registry does.
	if err := producer.TombstonePrice(ctx, marketID(t, doomed), kafka.Tombstone{
		Reason:      reason,
		Acknowledge: kafka.AcknowledgeDeletesKeyFromSnapshot,
	}); err != nil {
		t.Fatalf("TombstonePrice on a compacted topic = %v, want it to be allowed", err)
	}

	s := newSnapshotter(t, bus, kafka.TopicOddsNormalized)

	// The tombstone is self-describing even though it has no body — that is the
	// whole reason the envelope's identifying fields are duplicated as headers.
	var tombstones int
	readCtx, readCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer readCancel()

	if _, err := s.Read(readCtx, func(_ context.Context, d *kafka.Delivery) error {
		if d.Key != doomed || !d.Tombstone {
			return nil
		}
		tombstones++
		if d.TombstoneReason != reason {
			t.Errorf("tombstone reason = %q, want %q", d.TombstoneReason, reason)
		}
		if d.Headers[kafka.HeaderTombstone] != "1" {
			t.Errorf("tombstone at offset %d has %s=%q, want \"1\"",
				d.Offset, kafka.HeaderTombstone, d.Headers[kafka.HeaderTombstone])
		}
		if d.Headers[kafka.HeaderProducer] != itService {
			t.Errorf("tombstone at offset %d names producer %q; an anonymous deletion is unauditable",
				d.Offset, d.Headers[kafka.HeaderProducer])
		}
		if d.Value() != nil {
			t.Errorf("the tombstone carries a %d-byte value; a deletion is a NULL value", len(d.Value()))
		}
		// Unmarshalling a deletion is a caller bug and says so, rather than
		// quietly producing an empty struct.
		var p itPayload
		if err := d.Unmarshal(&p); !errors.Is(err, kafka.ErrMalformedEnvelope) {
			t.Errorf("Unmarshal on a tombstone = %v, want ErrMalformedEnvelope", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("Snapshotter.Read: %v", err)
	}
	if tombstones != 1 {
		t.Fatalf("read %d tombstones for %s, want 1", tombstones, doomed)
	}

	// The fold drops it immediately.
	foldCtx, foldCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer foldCancel()

	state, stats, err := s.Snapshot(foldCtx)
	if err != nil {
		t.Fatalf("Snapshotter.Snapshot: %v", err)
	}
	if _, present := state[doomed]; present {
		t.Error("the tombstoned key is still in the folded snapshot; a consumer that ignored it would " +
			"serve a price for a market that is no longer offered")
	}
	if raw, ok := state[kept]; !ok {
		t.Errorf("the untouched key is missing from the snapshot")
	} else {
		var p itPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode the folded payload for %s: %v", kept, err)
		}
		if p.Version != 2 {
			t.Errorf("the untouched key folded to version %d, want 2", p.Version)
		}
	}
	if stats.Tombstones < 1 {
		t.Errorf("the snapshot reported %d tombstones, want at least the one this test wrote", stats.Tombstones)
	}

	// And the log itself converges: the doomed key's values are collected,
	// leaving at most the tombstone.
	time.Sleep(1500 * time.Millisecond)
	publishNormalized(t, producer, uniqueID("mkt-roll"), 1)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		values, _, _ := snapshotKeyCounts(t, s, mine)
		if values[doomed] == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("the tombstoned key's values were never collected by the log cleaner; a replay from scratch " +
		"would resurrect a deleted market")
}

// =============================================================================
// 7. Consumer groups: rebalancing, offsets, at-least-once
// =============================================================================

// assignedPartitions reads a member's own view of how many partitions it owns.
func assignedPartitions(t *testing.T, bus busOptions, group, topic string) int {
	t.Helper()
	return int(busMetric(t, bus.registry, "sharpline_kafka_partitions_assigned",
		map[string]string{"group": group, "topic": topic}))
}

// TestConsumerGroupRebalanceLosesNothingAndRepeatsNothingCommitted is the test
// CLAUDE.md §10 is pointing at when it says the interesting bugs live in
// consumer-group rebalancing and offset handling.
//
// Two members share a topic's partitions. One leaves. The survivor must pick up
// the departed member's partitions, must NOT re-deliver anything the group had
// already committed, and must NOT skip anything the group had not. Both halves
// are asserted as exact counts, because "it did not crash" is compatible with
// silently losing a partition's worth of odds updates.
//
// The members are kafka.Consumer values now rather than hand-written poll loops,
// so what is under test is the shipped commit boundary: auto-commit disabled,
// rebalances blocked on poll, and exactly the records a Handler returned nil for
// committed. The assignment is read from sharpline_kafka_partitions_assigned —
// the consumer exposes it nowhere else, and it is the same series the dashboard
// aggregates.
func TestConsumerGroupRebalanceLosesNothingAndRepeatsNothingCommitted(t *testing.T) {
	t.Parallel()

	const partitions = 4
	provider, topic := newRawTopic(t, partitions)
	group := uniqueID("sharpline-it-group")

	busA := newBusOptions(t)
	busB := newBusOptions(t)
	busP := newBusOptions(t)

	producer := newOddsProducer(t, busP)

	const perRound = 24
	publishRound := func(round int) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		for i := 0; i < perRound; i++ {
			key := fmt.Sprintf("evt-r%d-%02d", round, i)
			if err := producer.PublishRaw(ctx, provider, eventID(t, key),
				kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: key, Version: round}}); err != nil {
				t.Fatalf("PublishRaw(%s): %v", key, err)
			}
		}
	}

	log := newDeliveryLog()
	configure := func(o *kafka.ConsumerOptions) {
		o.Group = group
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
		o.LagRefreshInterval = 500 * time.Millisecond
	}

	// A is observed entirely through its own metrics registry — the shipped
	// Consumer exposes its assignment nowhere else — so the handle is not needed
	// beyond the cleanup startMember already registers.
	startMember(t, busA, "A", configure, log.handler(t, "A"))
	memberB := startMember(t, busB, "B", configure, log.handler(t, "B"))

	// Produce only once BOTH members own partitions. Producing first would let
	// whichever member joined earliest consume the whole batch before the second
	// one was assigned anything, and the hand-off that follows would then be a
	// hand-off of nothing.
	awaitTrue(t, 60*time.Second, "both members own at least one partition", func() bool {
		return assignedPartitions(t, busA, group, topic) > 0 &&
			assignedPartitions(t, busB, group, topic) > 0
	})
	ownedA := assignedPartitions(t, busA, group, topic)
	ownedB := assignedPartitions(t, busB, group, topic)
	if ownedA+ownedB != partitions {
		t.Fatalf("the group owns %d partitions between its two members (A=%d, B=%d), want %d",
			ownedA+ownedB, ownedA, ownedB, partitions)
	}

	publishRound(1)
	awaitTrue(t, 90*time.Second, "round 1 was delivered", func() bool { return log.distinct() >= perRound })

	// Both members must have done work. If one took every partition, the test
	// that follows would prove nothing about a hand-off.
	totals := log.memberTotals()
	if totals["A"] == 0 || totals["B"] == 0 {
		t.Fatalf("partitions were assigned to both members but only one consumed: %v (A owns %d, B owns %d)",
			totals, ownedA, ownedB)
	}
	t.Logf("round 1 delivered %d records across 2 members: %v (A owns %d partitions, B owns %d)",
		log.distinct(), totals, ownedA, ownedB)

	// The consume loop commits after every batch, so waiting for the group's lag
	// to reach zero is waiting for the "already committed" half of the claim to
	// become true — an outcome rather than a sleep.
	awaitTrue(t, 60*time.Second, "round 1 is fully committed", func() bool {
		return groupLag(t, group) == 0
	})
	round1 := log.coords()
	rebalancesBefore := busMetric(t, busA.registry, "sharpline_kafka_consumer_group_rebalances_total",
		map[string]string{"group": group})

	// ---- the departure ----------------------------------------------------
	if err := memberB.shutdown(t); err != nil {
		t.Fatalf("B's Run returned %v, want a clean stop", err)
	}

	// The survivor must be handed every partition. Asserting the ASSIGNMENT
	// separately from the deliveries means a failure distinguishes "the rebalance
	// never happened" from "it happened and records went missing".
	awaitTrue(t, 60*time.Second, "A took over every partition", func() bool {
		return assignedPartitions(t, busA, group, topic) == partitions
	})
	rebalancesAfter := busMetric(t, busA.registry, "sharpline_kafka_consumer_group_rebalances_total",
		map[string]string{"group": group})
	if rebalancesAfter <= rebalancesBefore {
		t.Errorf("A observed no new rebalance after B left (%v, was %v); the takeover was not a rebalance",
			rebalancesAfter, rebalancesBefore)
	}

	// Everything published now must reach the survivor, which requires it to have
	// taken over B's partitions.
	publishRound(2)
	awaitTrue(t, 120*time.Second, "round 2 was delivered after the rebalance", func() bool {
		return log.distinct() >= 2*perRound
	})

	// ---- nothing committed was reprocessed --------------------------------
	after := log.coords()
	var reprocessed []string
	for coord := range round1 {
		if after[coord] > 1 {
			reprocessed = append(reprocessed, coord)
		}
	}
	if len(reprocessed) > 0 {
		t.Errorf("%d records committed before the rebalance were delivered again: %v",
			len(reprocessed), reprocessed)
	}

	// ---- nothing was lost -------------------------------------------------
	if got := log.distinct(); got != 2*perRound {
		t.Errorf("the group delivered %d distinct records across both rounds, want %d", got, 2*perRound)
	}

	// ---- and the survivor really owns every partition ---------------------
	awaitTrue(t, 60*time.Second, "the group is caught up after round 2", func() bool {
		return groupLag(t, group) == 0
	})

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	lags, err := adm.Lag(ctx, group)
	if err != nil {
		t.Fatalf("describe group lag: %v", err)
	}
	lag, ok := lags[group]
	if !ok {
		t.Fatalf("group %s is not described by the coordinator", group)
	}
	if err := lag.Error(); err != nil {
		t.Fatalf("group %s lag: %v", group, err)
	}
	if n := len(lag.Members); n != 1 {
		t.Errorf("the group has %d members after the departure, want 1", n)
	}

	var describedPartitions int
	for _, byPartition := range lag.Lag {
		describedPartitions += len(byPartition)
	}
	if describedPartitions != partitions {
		t.Errorf("the coordinator describes %d partitions, want %d", describedPartitions, partitions)
	}
}

// groupLag is the group's total lag across every partition, or -1 when the
// coordinator cannot describe it yet.
//
// A negative sentinel rather than a fatal: it is polled while a group is
// forming, and "not described yet" is a normal transient state there.
func groupLag(t *testing.T, group string) int64 {
	t.Helper()

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	lags, err := adm.Lag(ctx, group)
	if err != nil {
		return -1
	}
	described, ok := lags[group]
	if !ok || described.Error() != nil {
		return -1
	}

	var total int64
	for _, byPartition := range described.Lag {
		for _, l := range byPartition {
			if l.Err != nil {
				return -1
			}
			total += l.Lag
		}
	}
	return total
}

// TestErrorPolicyStopCommitsTheHandledPrefixAndRedeliversTheRest covers two
// claims at once, because they are the same mechanism seen from two sides.
//
// ErrorPolicyStop is the ZERO VALUE and therefore the default: "the alternative
// default silently drops data, and a silent default that loses records is not a
// default anybody would choose deliberately." When a record fails, the consumer
// stops with THAT record's offset uncommitted — but "records handled
// successfully BEFORE the failure are still committed, so stopping costs no
// rework beyond the failing record itself."
//
// And that is also the at-least-once guarantee doc.go chooses for every topic:
// a member that handles records and stops before committing them causes
// REDELIVERY, never loss. The replacement here must see the failing record and
// everything after it, and must NOT see the prefix.
func TestErrorPolicyStopCommitsTheHandledPrefixAndRedeliversTheRest(t *testing.T) {
	t.Parallel()

	// One partition, so "the first two records" is unambiguous.
	provider, topic := newRawTopic(t, 1)
	group := uniqueID("sharpline-it-group")

	busP := newBusOptions(t)
	producer := newOddsProducer(t, busP)

	const total = 6
	const failAt = 2 // zero-based: two records are handled, the third fails

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("evt-%02d", i)
		if err := producer.PublishRaw(ctx, provider, eventID(t, key),
			kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: key, Version: i}}); err != nil {
			t.Fatalf("PublishRaw(%s): %v", key, err)
		}
	}

	// ---- the first member handles two records and then refuses ------------
	busA := newBusOptions(t)
	poison := errors.New("the third record is poison")

	var handled []int64
	var mu sync.Mutex
	first := startMember(t, busA, "first", func(o *kafka.ConsumerOptions) {
		o.Group = group
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
		// ErrorPolicy is deliberately left unset: the zero value IS
		// ErrorPolicyStop, and a test that spelled it out would not notice if the
		// default moved.
	}, kafka.HandlerFunc(func(_ context.Context, d *kafka.Delivery) error {
		mu.Lock()
		defer mu.Unlock()
		if d.Offset >= failAt {
			return poison
		}
		handled = append(handled, d.Offset)
		return nil
	}))

	runErr := first.awaitRunError(t, 90*time.Second)
	if !errors.Is(runErr, kafka.ErrHandlerFailed) {
		t.Fatalf("Run returned %v, want ErrHandlerFailed; under ErrorPolicyStop a failing record must "+
			"stop the consumer rather than be skipped", runErr)
	}
	if !errors.Is(runErr, poison) {
		t.Errorf("Run's error does not wrap the handler's own: %v", runErr)
	}
	if !strings.Contains(runErr.Error(), topic) {
		t.Errorf("Run's error does not name the record's topic: %v", runErr)
	}

	mu.Lock()
	handledCount := len(handled)
	mu.Unlock()
	if handledCount != failAt {
		t.Fatalf("the first member handled %d records before failing, want %d", handledCount, failAt)
	}

	// Leave the group so the replacement is not waiting out a session timeout.
	if err := first.Close(); err != nil {
		t.Fatalf("close the first member: %v", err)
	}

	if n := busMetric(t, busA.registry, "sharpline_kafka_handler_errors_total",
		map[string]string{"group": group, "topic": topic}); n < 1 {
		t.Errorf("handler_errors_total = %v, want at least 1", n)
	}

	// ---- the replacement must see exactly the uncommitted tail ------------
	busB := newBusOptions(t)
	log := newDeliveryLog()
	startMember(t, busB, "replacement", func(o *kafka.ConsumerOptions) {
		o.Group = group
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, log.handler(t, "replacement"))

	awaitTrue(t, 90*time.Second, "the uncommitted tail was redelivered", func() bool {
		return log.distinct() >= total-failAt
	})

	// Give it a beat to prove it does NOT also redeliver the committed prefix.
	time.Sleep(3 * time.Second)

	if got, want := log.distinct(), total-failAt; got != want {
		t.Fatalf("the replacement received %d distinct records, want exactly %d "+
			"(the %d successfully handled records must not be redelivered, and none of the %d "+
			"uncommitted may be lost)", got, want, failAt, want)
	}

	delivered := log.coords()
	for offset := 0; offset < failAt; offset++ {
		if delivered[fmt.Sprintf("0/%d", offset)] > 0 {
			t.Errorf("record at offset %d was handled successfully by the first member and redelivered; "+
				"the prefix commit did not take", offset)
		}
	}
	for offset := failAt; offset < total; offset++ {
		if delivered[fmt.Sprintf("0/%d", offset)] == 0 {
			t.Errorf("record at offset %d was LOST; at-least-once means an uncommitted record must be "+
				"redelivered", offset)
		}
	}
}

// TestCommitOutlivesTheCallersCancellation covers the one line in commitPending
// that a shutdown depends on.
//
// consumer.go: "By the time this runs the work IS done. Abandoning the commit
// because ctx was cancelled guarantees the next member redoes it, which is the
// exact failure at-least-once is supposed to make rare rather than routine. So
// the caller's cancellation is dropped (context.WithoutCancel keeps the values,
// including trace context) and CommitTimeout is what actually bounds it."
//
// Deleting the WithoutCancel is a one-token edit and every other test in this
// file would still pass: the batch commits that happen mid-run use a live
// context. Only the commit that runs while the consumer is being shut down uses
// a cancelled one, so that is the commit this test forces.
//
// The handler is slow on purpose. Run's context is cancelled while a batch is
// still being handled, so by the time handleFetches reaches its commit the
// context is already dead — and if the cancellation propagated, nothing would be
// committed and the whole batch would be redelivered.
func TestCommitOutlivesTheCallersCancellation(t *testing.T) {
	t.Parallel()

	provider, topic := newRawTopic(t, 1)
	group := uniqueID("sharpline-it-group")

	busP := newBusOptions(t)
	producer := newOddsProducer(t, busP)

	const total = 40

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("evt-%02d", i)
		if err := producer.PublishRaw(ctx, provider, eventID(t, key),
			kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: key, Version: i}}); err != nil {
			t.Fatalf("PublishRaw(%s): %v", key, err)
		}
	}

	bus := newBusOptions(t)
	var mu sync.Mutex
	var handledOffsets []int64
	started := make(chan struct{})
	var once sync.Once

	m := startMember(t, bus, "shutdown", func(o *kafka.ConsumerOptions) {
		o.Group = group
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
		// One poll takes the whole batch, so the cancellation below lands inside
		// it rather than between two of them.
		o.MaxPollRecords = total
	}, kafka.HandlerFunc(func(_ context.Context, d *kafka.Delivery) error {
		once.Do(func() { close(started) })
		time.Sleep(25 * time.Millisecond)

		mu.Lock()
		handledOffsets = append(handledOffsets, d.Offset)
		mu.Unlock()
		return nil
	}))

	select {
	case <-started:
	case <-time.After(90 * time.Second):
		t.Fatal("the consumer never received the batch")
	}

	// Cancel while the batch is still in flight. The handler ignores the context,
	// so the batch finishes and the commit that follows runs on a dead one.
	time.Sleep(100 * time.Millisecond)
	if err := m.shutdown(t); err != nil {
		t.Fatalf("Run returned %v, want a clean stop on cancellation", err)
	}

	mu.Lock()
	handled := len(handledOffsets)
	mu.Unlock()
	if handled == 0 {
		t.Fatal("nothing was handled before the shutdown; the test proved nothing")
	}

	if n := busMetric(t, bus.registry, "sharpline_kafka_offset_commits_total",
		map[string]string{"group": group, "outcome": "ok"}); n < 1 {
		t.Fatalf("the consumer committed nothing across the shutdown (%v successful commits). "+
			"commitPending's context.WithoutCancel is what stops a cancelled shutdown from "+
			"discarding work that was already done", n)
	}

	// The group's committed offset must be exactly what was handled. On one
	// partition starting at offset zero, the committed offset IS the number of
	// records the group is done with.
	//
	// This is decisive whatever the poll happened to return: every commit that
	// runs after the cancellation above runs on a dead context, and the last one
	// is the one that carries the batch the handler was in the middle of. Drop
	// the WithoutCancel and that commit fails, so the committed offset falls
	// short of the work that was actually finished.
	committed := groupCommitted(t, group, topic)
	if committed != int64(handled) {
		t.Fatalf("the group has committed offset %d after handling %d records; a shutdown that drops "+
			"its commit forces the next member to redo committed work", committed, handled)
	}
	t.Logf("handled %d records and committed offset %d across a cancelled shutdown, in %v successful commit(s)",
		handled, committed,
		busMetric(t, bus.registry, "sharpline_kafka_offset_commits_total",
			map[string]string{"group": group, "outcome": "ok"}))
}

// groupCommitted is the group's committed offset summed over a topic's
// partitions.
func groupCommitted(t *testing.T, group, topic string) int64 {
	t.Helper()

	adm := newKafkaAdmin(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	offsets, err := adm.FetchOffsets(ctx, group)
	if err != nil {
		t.Fatalf("fetch committed offsets for %s: %v", group, err)
	}
	if err := offsets.Error(); err != nil {
		t.Fatalf("fetch committed offsets for %s: %v", group, err)
	}

	var total int64
	for _, byPartition := range offsets.Offsets() {
		for _, o := range byPartition {
			if o.Topic != topic {
				continue
			}
			total += o.At
		}
	}
	return total
}

// =============================================================================
// 8. Snapshot mode
// =============================================================================

// TestSnapshotReadTerminatesAtTheEndOffsets checks the property that makes a
// snapshot read usable on a startup path at all.
//
// A compacted topic is read from the beginning to build current state
// (CLAUDE.md §3). If that read tailed forever, a service would never finish
// starting — the Kubernetes startupProbe would kill it, and on a live topic there
// is always another record arriving. So the read must terminate at the high
// watermark it observed when it began, and records produced after that point must
// belong to the NEXT read rather than extending this one.
//
// The watermark is captured INSIDE Snapshotter.Read now rather than by the test,
// which is the point of driving the shipped type — so the assertion is framed on
// what is observable from outside: a key whose ack landed after Read RETURNED
// cannot possibly have been part of the snapshot, and a read that was tailing
// would still be running when the writer stopped.
func TestSnapshotReadTerminatesAtTheEndOffsets(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	const initial = 12
	before := make([]string, 0, initial)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	for i := 0; i < initial; i++ {
		key := uniqueID("mkt-before")
		before = append(before, key)
		if err := producer.PublishPrice(ctx, marketID(t, key), itMessage(key, 1, time.Now().UTC())); err != nil {
			t.Fatalf("PublishPrice(%s): %v", key, err)
		}
	}

	// A live topic keeps moving while the snapshot is being read. This writer
	// exists so that "terminated" means something stronger than "ran out of data".
	//
	// It produces from its own goroutine, so it must not call t.Fatalf — it
	// records the first failure and the test inspects it afterwards.
	var (
		writerMu   sync.Mutex
		writerKeys []string
		writerErr  error
		writerDone = make(chan struct{})
		writerStop = make(chan struct{})
	)
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-writerStop:
				return
			default:
			}

			key := uniqueID("mkt-live")
			produceCtx, produceCancel := context.WithTimeout(context.Background(), 20*time.Second)
			err := producer.PublishPrice(produceCtx, marketID(t, key), itMessage(key, 2, time.Now().UTC()))
			produceCancel()

			writerMu.Lock()
			if err != nil {
				writerErr = err
				writerMu.Unlock()
				return
			}
			writerKeys = append(writerKeys, key)
			writerMu.Unlock()

			time.Sleep(20 * time.Millisecond)
		}
	}()
	t.Cleanup(func() {
		select {
		case <-writerStop:
		default:
			close(writerStop)
		}
		<-writerDone
	})

	// Let the writer get ahead, so the snapshot has a genuine tail to refuse
	// rather than an empty one.
	time.Sleep(300 * time.Millisecond)

	writerMu.Lock()
	publishedBeforeRead := len(writerKeys)
	writerMu.Unlock()

	s := newSnapshotter(t, bus, kafka.TopicPriceComputed)

	readCtx, readCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer readCancel()

	start := time.Now()
	state, stats, err := s.Snapshot(readCtx)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Snapshotter.Snapshot: %v", err)
	}

	// Everything the writer produced from here on is unambiguously after the read
	// finished, so none of it may be in the snapshot.
	writerMu.Lock()
	publishedAtReadEnd := len(writerKeys)
	writerMu.Unlock()

	time.Sleep(300 * time.Millisecond)
	close(writerStop)
	<-writerDone

	writerMu.Lock()
	after := append([]string(nil), writerKeys...)
	failure := writerErr
	writerMu.Unlock()

	if failure != nil {
		t.Fatalf("the concurrent writer failed: %v", failure)
	}
	if len(after) <= publishedAtReadEnd {
		t.Fatalf("the writer published nothing after the read returned (%d then, %d now); the read "+
			"cannot be distinguished from one that simply ran out of records",
			publishedAtReadEnd, len(after))
	}
	t.Logf("the writer had published %d records when the read began and %d when it returned, "+
		"and kept going to %d afterwards", publishedBeforeRead, publishedAtReadEnd, len(after))

	// THE TERMINATION CLAIM. The topic ends up holding more records than the
	// snapshot returned, so the read stopped at the watermark it observed rather
	// than at "wherever the topic happened to stop" — which on a live topic is
	// never.
	if grown := topicRecordCount(t, kafka.TopicPriceComputed); grown <= int64(stats.Records()) {
		t.Errorf("the topic holds %d records and the snapshot returned %d; it did not stop short of "+
			"the tail, so nothing here distinguishes terminating from running out of data",
			grown, stats.Records())
	}

	for _, key := range before {
		if _, ok := state[key]; !ok {
			t.Errorf("key %s existed before the snapshot began and is missing from it", key)
		}
	}
	for _, key := range after[publishedAtReadEnd:] {
		if _, ok := state[key]; ok {
			t.Errorf("key %s was produced AFTER the snapshot returned and must not be in it", key)
		}
	}

	if stats.Records() < initial {
		t.Errorf("the snapshot reported %d records, want at least the %d that existed before it began",
			stats.Records(), initial)
	}
	t.Logf("snapshot of %d keys (%d records across %d partitions) terminated in %s while the topic "+
		"kept being written", len(state), stats.Records(), stats.Partitions, elapsed.Round(time.Millisecond))

	// A snapshot read uses NO consumer group, so it commits nothing and disturbs
	// no live consumer. If it had joined one, the coordinator would know about it.
	adm := newKafkaAdmin(t)
	listCtx, listCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer listCancel()

	groups, err := adm.ListGroups(listCtx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for name := range groups {
		if strings.Contains(name, kafka.TopicPriceComputed) || strings.Contains(name, "snapshot") {
			t.Errorf("a consumer group %q exists that names the snapshot topic; a snapshot read must "+
				"be groupless", name)
		}
	}
}

// =============================================================================
// 9. The audit trail, and readiness
// =============================================================================

// TestAuditProducerWritesTheSettlementTrail drives the other producer type end
// to end.
//
// wager.events is retention-based and keyed by wager, so a wager's placement,
// cash-out and settlement stay ordered relative to each other and superseded
// entries SURVIVE — "the whole value of an audit trail is that superseded
// entries survive", which is why the topic is not compacted and why this
// producer has no tombstone method.
func TestAuditProducerWritesTheSettlementTrail(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	audit := newAuditProducer(t, bus)

	wager := wagerID(t, uniqueID("wgr"))
	stages := []string{"wager.placed.v1", "wager.graded.v1", "wager.settled.v1"}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	for i, stage := range stages {
		if err := audit.PublishWagerEvent(ctx, wager, kafka.Message{
			Type:    stage,
			ID:      fmt.Sprintf("%s-%d", wager, i),
			Payload: itPayload{Market: wager.String(), Version: i},
		}); err != nil {
			t.Fatalf("PublishWagerEvent(%s): %v", stage, err)
		}
	}

	var mu sync.Mutex
	var seen []string
	var wrongKind error

	startMember(t, bus, "settle", func(o *kafka.ConsumerOptions) {
		o.Group = uniqueID("sharpline-it-group")
		o.Topics = []string{kafka.TopicWagerEvents}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, kafka.HandlerFunc(func(_ context.Context, d *kafka.Delivery) error {
		if d.Key != wager.String() {
			return nil
		}
		id, err := d.WagerID()
		if err != nil {
			t.Errorf("WagerID() on wager.events = %v, want the key back", err)
		}
		if id != wager {
			t.Errorf("WagerID() = %q, want %q", id, wager)
		}
		_, mErr := d.MarketID()

		mu.Lock()
		seen = append(seen, d.Envelope.Type)
		wrongKind = mErr
		mu.Unlock()
		return nil
	}))

	awaitTrue(t, 90*time.Second, "every stage of the wager's life reached the consumer", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= len(stages)
	})

	mu.Lock()
	got := append([]string(nil), seen...)
	kindErr := wrongKind
	mu.Unlock()

	if len(got) != len(stages) {
		t.Fatalf("the consumer saw %d events for this wager, want %d — an audit trail that collapses "+
			"to its final state is not an audit trail: %v", len(got), len(stages), got)
	}
	for i, want := range stages {
		if got[i] != want {
			t.Errorf("event %d is %q, want %q; wager.events is keyed by wager so one wager's "+
				"lifecycle is ordered", i, got[i], want)
		}
	}
	if !errors.Is(kindErr, kafka.ErrWrongKeyKind) {
		t.Errorf("MarketID() on wager.events = %v, want ErrWrongKeyKind", kindErr)
	}
}

// TestReadinessAnswersTheClusterQuestionForEveryClient covers the /readyz half of
// every bus client against a cluster that is actually there.
//
// The failing side has a unit test; this is the side that cannot be faked — a
// Check that returns nil because it is a real ApiVersions round trip to a real
// broker, and an ErrClosed the moment the client is shut down.
func TestReadinessAnswersTheClusterQuestionForEveryClient(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	_, topic := newRawTopic(t, 1)

	openCtx, openCancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer openCancel()

	odds, err := kafka.NewOddsProducer(openCtx, kafka.ProducerOptions{ClientOptions: bus.ClientOptions})
	if err != nil {
		t.Fatalf("NewOddsProducer: %v", err)
	}
	audit, err := kafka.NewAuditProducer(openCtx, kafka.ProducerOptions{ClientOptions: bus.ClientOptions})
	if err != nil {
		t.Fatalf("NewAuditProducer: %v", err)
	}
	consumer, err := kafka.NewConsumer(openCtx, kafka.ConsumerOptions{
		ClientOptions: bus.ClientOptions,
		Group:         uniqueID("sharpline-it-group"),
		Topics:        []string{topic},
	})
	if err != nil {
		t.Fatalf("NewConsumer: %v", err)
	}
	snapshotter, err := kafka.NewSnapshotter(openCtx, kafka.SnapshotOptions{
		ClientOptions: bus.ClientOptions,
		Topic:         kafka.TopicOddsNormalized,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	type namedChecker struct {
		name  string
		check func(context.Context) error
		ping  func(context.Context) error
		close func() error
	}
	clients := []namedChecker{
		{"OddsProducer", odds.Check, odds.Ping, odds.Close},
		{"AuditProducer", audit.Check, audit.Ping, audit.Close},
		{"Consumer", consumer.Check, consumer.Ping, consumer.Close},
		{"Snapshotter", snapshotter.Check, snapshotter.Ping, snapshotter.Close},
	}

	if odds.Name() != "kafka" {
		t.Errorf("Name() = %q, want %q — it is the key the dependency appears under in /readyz",
			odds.Name(), "kafka")
	}

	for _, c := range clients {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		if err := c.check(ctx); err != nil {
			t.Errorf("%s.Check() against a live cluster = %v, want nil", c.name, err)
		}
		if err := c.ping(ctx); err != nil {
			t.Errorf("%s.Ping() against a live cluster = %v, want nil", c.name, err)
		}
		cancel()
	}

	for _, c := range clients {
		if err := c.close(); err != nil {
			t.Errorf("%s.Close(): %v", c.name, err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		if err := c.check(ctx); !errors.Is(err, kafka.ErrClosed) {
			t.Errorf("%s.Check() after Close = %v, want ErrClosed; during a rolling update the "+
				"payload has to say the process is shutting down", c.name, err)
		}
		cancel()
	}

	// The up gauge tracked a live cluster the whole way through.
	if got := busMetric(t, bus.registry, "sharpline_kafka_up", nil); got != 1 {
		t.Errorf("sharpline_kafka_up = %v after a series of successful probes, want 1", got)
	}
}

// TestAsyncPublishOnTheCompactedTopicsReachesTheLog covers the two asynchronous
// publishers the odds path uses at rate.
//
// The synchronous forms are exercised everywhere in this file; these are the
// ones a pricer running at slate speed actually calls, and the property that
// matters is the boring one — a record accepted asynchronously and then flushed
// is in the log, under its own key, with the callback reporting success.
func TestAsyncPublishOnTheCompactedTopicsReachesTheLog(t *testing.T) {
	t.Parallel()

	declaredKafkaTopics(t)

	bus := newBusOptions(t)
	producer := newOddsProducer(t, bus)

	normalized := uniqueID("mkt-async-normalized")
	priced := uniqueID("mkt-async-priced")

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	acks := make(chan error, 2)
	if err := producer.PublishNormalizedAsync(ctx, marketID(t, normalized),
		itMessage(normalized, 7, time.Now().UTC()), func(err error) { acks <- err }); err != nil {
		t.Fatalf("PublishNormalizedAsync: %v", err)
	}
	if err := producer.PublishPriceAsync(ctx, marketID(t, priced),
		itMessage(priced, 9, time.Now().UTC()), func(err error) { acks <- err }); err != nil {
		t.Fatalf("PublishPriceAsync: %v", err)
	}

	if err := producer.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	for i := 0; i < 2; i++ {
		select {
		case err := <-acks:
			if err != nil {
				t.Fatalf("an async publish failed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("Flush returned but an async publish never settled its promise")
		}
	}

	for _, tc := range []struct {
		topic   string
		key     string
		version int
	}{
		{kafka.TopicOddsNormalized, normalized, 7},
		{kafka.TopicPriceComputed, priced, 9},
	} {
		s := newSnapshotter(t, bus, tc.topic)
		snapCtx, snapCancel := context.WithTimeout(t.Context(), 90*time.Second)
		state, _, err := s.Snapshot(snapCtx)
		snapCancel()
		if err != nil {
			t.Fatalf("Snapshotter.Snapshot(%s): %v", tc.topic, err)
		}
		raw, ok := state[tc.key]
		if !ok {
			t.Errorf("%s is missing from %s after an async publish and a flush", tc.key, tc.topic)
			continue
		}
		var p itPayload
		if err := json.Unmarshal(raw, &p); err != nil {
			t.Fatalf("decode the folded payload for %s: %v", tc.key, err)
		}
		if p.Version != tc.version {
			t.Errorf("%s folded to version %d, want %d", tc.key, p.Version, tc.version)
		}
	}
}

// TestAnUndecodableRecordStopsOrIsSkippedByPolicy covers the branch of the
// consume loop that a well-behaved producer can never reach.
//
// consumer.go: "Skipping is the only way past an undecodable record: retrying it
// cannot change the bytes on disk." That is the argument for ErrorPolicySkip
// existing at all, and it is only checkable against a record this package would
// refuse to write — encode rejects a nil, empty or "null" payload, and the only
// way to a null value is the Tombstone API. So the bad record is written with a
// RAW client, which is the second and last raw produce in this file.
//
// Both policies are exercised on the same three records, with a fresh consumer
// group each, because the interesting thing is the DIFFERENCE: the default stops
// with the record uncommitted, and the opt-in advances past it.
func TestAnUndecodableRecordStopsOrIsSkippedByPolicy(t *testing.T) {
	t.Parallel()

	provider, topic := newRawTopic(t, 1)

	busP := newBusOptions(t)
	producer := newOddsProducer(t, busP)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	publish := func(key string, version int) {
		t.Helper()
		if err := producer.PublishRaw(ctx, provider, eventID(t, key),
			kafka.Message{Type: "odds.raw.v1", Payload: itPayload{Market: key, Version: version}}); err != nil {
			t.Fatalf("PublishRaw(%s): %v", key, err)
		}
	}

	publish("evt-good-0", 0)

	// Offset 1: bytes no decoder can rescue. Written raw, because the shipped
	// producer's whole job is to make this impossible.
	raw := newKafkaClient(t)
	if err := raw.ProduceSync(ctx, &kgo.Record{
		Topic: topic,
		Key:   []byte("evt-corrupt"),
		Value: []byte(`{"v":1,"type":`), // truncated mid-document
	}).FirstErr(); err != nil {
		t.Fatalf("produce the corrupt record: %v", err)
	}

	publish("evt-good-2", 2)

	// ---- the default: stop, with the bad record uncommitted ---------------
	stopBus := newBusOptions(t)
	stopGroup := uniqueID("sharpline-it-group")
	stopLog := newDeliveryLog()

	stopper := startMember(t, stopBus, "stop", func(o *kafka.ConsumerOptions) {
		o.Group = stopGroup
		o.Topics = []string{topic}
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, stopLog.handler(t, "stop"))

	runErr := stopper.awaitRunError(t, 90*time.Second)
	if !errors.Is(runErr, kafka.ErrMalformedEnvelope) {
		t.Fatalf("Run returned %v, want ErrMalformedEnvelope; under the default policy an undecodable "+
			"record stops the consumer rather than being silently dropped", runErr)
	}
	if got := stopLog.distinct(); got != 1 {
		t.Errorf("the stopping consumer handled %d records, want only the good one before the corrupt "+
			"record", got)
	}
	if err := stopper.Close(); err != nil {
		t.Fatalf("close the stopping member: %v", err)
	}
	if n := busMetric(t, stopBus.registry, "sharpline_kafka_decode_errors_total",
		map[string]string{"group": stopGroup, "topic": topic, "reason": "malformed"}); n != 1 {
		t.Errorf("decode_errors_total{reason=malformed} = %v, want 1", n)
	}

	// ---- the opt-in: skip it, and keep going ------------------------------
	skipBus := newBusOptions(t)
	skipGroup := uniqueID("sharpline-it-group")
	skipLog := newDeliveryLog()

	startMember(t, skipBus, "skip", func(o *kafka.ConsumerOptions) {
		o.Group = skipGroup
		o.Topics = []string{topic}
		o.ErrorPolicy = kafka.ErrorPolicySkip
		o.SessionTimeout = 6 * time.Second
		o.HeartbeatInterval = time.Second
	}, skipLog.handler(t, "skip"))

	awaitTrue(t, 90*time.Second, "the skipping consumer got past the corrupt record", func() bool {
		return skipLog.keyCount("evt-good-2") > 0
	})

	if got := skipLog.keyCount("evt-corrupt"); got != 0 {
		t.Errorf("the corrupt record reached the handler %d times; a record that cannot be decoded "+
			"cannot be handled either", got)
	}
	if got := skipLog.keyCount("evt-good-0"); got != 1 {
		t.Errorf("the record before the corrupt one was delivered %d times, want 1", got)
	}
	if n := busMetric(t, skipBus.registry, "sharpline_kafka_decode_errors_total",
		map[string]string{"group": skipGroup, "topic": topic, "reason": "malformed"}); n != 1 {
		t.Errorf("decode_errors_total{reason=malformed} = %v, want 1; a skipped record must still be "+
			"counted, or the loss is invisible", n)
	}
}
