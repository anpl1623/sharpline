// The produce side: two producer types with two durability postures, and the
// write-side half of the key type-safety guarantee.
//
// doc.go argues the split; this file is the mechanism and the exact franz-go
// options each posture resolves to. Read doc.go's "Durability posture" section
// first — the table there is the specification this file implements.
//
// # Why the key check exists on BOTH sides
//
// envelope.go's Delivery.MarketID/EventID/WagerID refuse to hand back an
// identifier of the wrong kind on the READ side. That is only half the
// guarantee. A record published to odds.normalized under an EventID breaks two
// invariants simultaneously and neither one is loud:
//
//   - ORDERING. The key chooses the partition, and Kafka orders records only
//     within a partition. Two updates to one market keyed differently land on
//     different partitions and their relative order is lost.
//   - COMPACTION. The compacted log converges on one record per KEY. A market
//     written under two different keys becomes two entries that never collapse,
//     so the "current-line snapshot" CLAUDE.md §3 depends on holds a stale copy
//     for ever.
//
// The primary defence is the method signatures: PublishNormalized takes a
// domain.MarketID, so passing an EventID does not compile (this is the guarantee
// internal/domain/ids.go was written to provide, and it names the compacted
// Kafka key as one of the two reasons that file exists). recordKey and the
// runtime check in buildRecord are the second line: they catch a future publish
// method wired to the wrong topic, which the type system cannot see.
package kafka

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Producer geometry. Each constant is justified where it is used in
// oddsProducerOpts or auditProducerOpts.
const (
	// DefaultMaxBufferedRecords bounds the producer's in-memory buffer. Past
	// it, Produce blocks (and TryProduce fails), which is the point: an
	// unbounded buffer turns a slow broker into an OOM kill, and the deploy
	// target is a 2 OCPU / 12 GB box shared with Postgres, Redis, Kafka and
	// the whole service set.
	DefaultMaxBufferedRecords = 10_000

	// DefaultFlushTimeout bounds Flush and the flush Close performs. Chosen
	// under Kubernetes' default terminationGracePeriodSeconds (30s) so a pod
	// draining on SIGTERM finishes its flush before the runtime escalates to
	// SIGKILL, which would discard the buffer.
	DefaultFlushTimeout = 15 * time.Second

	// OddsRecordDeliveryTimeout is how long a record on an odds topic may keep
	// retrying before it is failed. Bounded, because a lost odds update is
	// self-healing — see oddsProducerOpts.
	OddsRecordDeliveryTimeout = 30 * time.Second

	// OddsRecordRetries bounds per-record retries on the odds path.
	OddsRecordRetries = 10

	// AuditLinger is the wager.events batching delay. Non-zero because that
	// path is synchronous and low-rate, so a few milliseconds of linger
	// coalesces the legs of one wager into one batch at no observable cost.
	AuditLinger = 5 * time.Millisecond
)

// -----------------------------------------------------------------------------
// Record keys
// -----------------------------------------------------------------------------

// recordKey is a record key paired with the kind of domain identifier it holds.
//
// It cannot be constructed from a bare string: the three constructors below each
// take a distinct domain type, so the only way to reach buildRecord is through a
// value the phase-1 identifier types already vouched for.
type recordKey struct {
	kind KeyKind
	id   string
}

// marketKey keys a record by market. odds.normalized and price.computed.
func marketKey(id domain.MarketID) recordKey {
	return recordKey{kind: KeyKindMarketID, id: string(id)}
}

// eventKey keys a record by event. odds.raw.{provider} — see OddsRaw for why
// the raw topics are keyed by event and not by market.
func eventKey(id domain.EventID) recordKey {
	return recordKey{kind: KeyKindEventID, id: string(id)}
}

// wagerKey keys a record by wager. wager.events.
func wagerKey(id domain.WagerID) recordKey {
	return recordKey{kind: KeyKindWagerID, id: string(id)}
}

// validate rejects a key that cannot choose a partition or identify a snapshot
// entry.
//
// A NULL key is a legitimate Kafka concept — it means "round-robin me across
// partitions" — and it is refused on every topic this package writes, because
// all four are keyed by design. On a compacted topic a null-keyed record is
// worse than useless: the log cleaner cannot compact it, so it accumulates for
// ever in a log whose whole purpose is to converge.
func (k recordKey) validate() error {
	if k.id == "" {
		return fmt.Errorf("%w: empty (a null key cannot be compacted and cannot order)", ErrInvalidKey)
	}
	if k.kind == KeyKindUnknown {
		return fmt.Errorf("%w: no key kind", ErrInvalidKey)
	}
	// The domain constructors already enforce this charset; re-checking costs
	// a scan of at most MaxIDLen bytes and closes the hole where a zero-valued
	// domain ID (the empty string is the zero value of every ID type) reaches
	// the bus without ever having been through a constructor.
	if len(k.id) > domain.MaxIDLen {
		return fmt.Errorf("%w: key is %d bytes, limit is %d", ErrInvalidKey, len(k.id), domain.MaxIDLen)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// ProducerOptions configures either producer type.
//
// It deliberately exposes NO durability knob. acks, idempotence, in-flight
// limits, linger and the delivery timeout are fixed by which constructor is
// called, because they are the whole content of the odds/audit distinction and
// a caller that could weaken them would eventually weaken them by accident. The
// two fields here are resource bounds, not correctness parameters.
type ProducerOptions struct {
	ClientOptions

	// MaxBufferedRecords bounds unacknowledged records held in memory. Zero
	// means DefaultMaxBufferedRecords.
	MaxBufferedRecords int

	// FlushTimeout bounds Flush and the flush performed by Close. Zero means
	// DefaultFlushTimeout.
	FlushTimeout time.Duration
}

func (o ProducerOptions) validate() error {
	if err := o.ClientOptions.validate(); err != nil {
		return err
	}
	if o.MaxBufferedRecords < 0 {
		return fmt.Errorf("%w: MaxBufferedRecords is negative", ErrInvalidOptions)
	}
	if o.FlushTimeout < 0 {
		return fmt.Errorf("%w: FlushTimeout is negative", ErrInvalidOptions)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Shared producer machinery
// -----------------------------------------------------------------------------

// producer holds everything the two producer types share. It is embedded, not
// exported, so that no caller can reach the generic publish path and thereby
// publish a wager event through the odds posture or vice versa.
type producer struct {
	healthChecker

	cl      *kgo.Client
	log     *slog.Logger
	m       *Metrics
	tracer  trace.Tracer
	prop    propagation.TextMapPropagator
	service string

	flushTimeout time.Duration

	// inflight tracks async promises so Close cannot return while a callback
	// is still running against a half-closed client.
	inflight sync.WaitGroup

	closed closedFlag
}

// newProducer builds the shared client. profile carries the per-posture
// options; everything else is identical by construction.
func newProducer(ctx context.Context, opts ProducerOptions, profile []kgo.Opt) (*producer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	m, err := opts.resolveMetrics()
	if err != nil {
		return nil, err
	}

	log := opts.Logger.With(slog.String("component", "kafka.producer"))

	p := &producer{
		log:          log,
		m:            m,
		tracer:       opts.tracer(),
		prop:         opts.propagator(),
		service:      opts.Service,
		flushTimeout: positiveOr(opts.FlushTimeout, DefaultFlushTimeout),
	}

	kopts := opts.baseOpts()
	kopts = append(kopts,
		// ---- durability, identical in both postures (doc.go) ----
		//
		// acks=all. On the single-broker KRaft development cluster the
		// replication factor is 1, so this costs exactly what acks=1 costs and
		// there is no latency argument for weakening it. Weakening it would
		// become silent data loss the day phase 10 runs Kafka as a StatefulSet
		// with RF>1 — the class of bug that is invisible until a broker dies.
		kgo.RequiredAcks(kgo.AllISRAcks()),

		// IDEMPOTENT PRODUCTION STAYS ON. It is franz-go's default and
		// kgo.DisableIdempotentWrite is never called anywhere in this package.
		// That is deliberate, and it is what makes a producer-side retry safe
		// in two separate ways:
		//
		//   - No duplicates. Without a producer id and per-partition sequence
		//     numbers, an ack lost on the way back produces a SECOND copy on
		//     retry — merely wasteful on a compacted topic, a phantom entry in
		//     an audit trail on wager.events.
		//   - No REORDERING. franz-go allows up to five concurrent produce
		//     requests per broker. With more than one in flight and no
		//     idempotence, request 1 can fail, request 2 succeed, and the retry
		//     of request 1 land after it — which on a compacted topic silently
		//     makes the OLDER record the latest for its key. That is a wrong
		//     line no amount of replay corrects. The broker's sequence check is
		//     what refuses the out-of-order batch and keeps ordering intact.
		//
		// The in-flight ceiling is deliberately NOT set here: franz-go pins it
		// at 5 while idempotence is on and REJECTS
		// kgo.MaxProduceRequestsInflightPerBroker outright ("invalid usage of
		// MaxProduceRequestsInflightPerBroker with idempotency enabled"), so
		// spelling the number out would fail every constructor in the process.
		// It becomes settable only by disabling idempotence, which is exactly
		// the combination that reorders.

		// lz4, falling back to none if the broker refuses. Not zstd: its Go
		// implementation costs more CPU per byte and the deploy target is a
		// 2 OCPU Ampere box. JSON from one topic is highly repetitive, so lz4
		// recovers most of the size difference against protobuf (doc.go).
		kgo.ProducerBatchCompression(kgo.Lz4Compression(), kgo.NoCompression()),

		// The murmur2 key partitioner Kafka's own clients use, set explicitly
		// rather than relied on as a default. A phase-12 Flink SQL job writing
		// these topics must land a given market_id on the SAME partition this
		// producer would, or the compacted log grows two entries for one market
		// and the snapshot is permanently wrong.
		kgo.RecordPartitioner(kgo.StickyKeyPartitioner(nil)),

		kgo.MaxBufferedRecords(positiveIntOr(opts.MaxBufferedRecords, DefaultMaxBufferedRecords)),

		// Data loss means a broker's log truncated beneath this producer's
		// sequence — what a leader failover with acks<all looks like. With
		// acks=all and idempotence on it must never fire, which is exactly why
		// it is wired to a counter and an error log rather than ignored.
		kgo.ProducerOnDataLossDetected(func(topic string, partition int32) {
			m.observeDataLoss(topic)
			log.Error("kafka producer detected data loss; records were written and then lost",
				slog.String("topic", topic),
				slog.Int("partition", int(partition)),
			)
		}),

		kgo.WithHooks(&produceBatchHook{m: m}),
	)
	kopts = append(kopts, profile...)

	cl, err := kgo.NewClient(kopts...)
	if err != nil {
		return nil, fmt.Errorf("%w: build producer client: %w", ErrInvalidOptions, err)
	}
	p.cl = cl
	p.healthChecker = healthChecker{
		cl:      cl,
		m:       m,
		log:     log,
		timeout: positiveOr(opts.ProbeTimeout, DefaultProbeTimeout),
		closed:  &p.closed,
	}

	if !opts.SkipStartupProbe {
		if err := awaitReady(ctx, cl, opts.ClientOptions, m, log); err != nil {
			cl.Close()
			return nil, err
		}
	}
	return p, nil
}

// produceBatchHook feeds sharpline_kafka_produce_batch_records.
//
// The batch size is not observable from the Produce call site — batching is
// franz-go's business and happens after the record is buffered — so it comes
// from the one place that knows it. It exists to CHECK rather than assume
// doc.go's claim that linger=0 still batches: a high produce rate must show
// batches well above 1, or the claim is wrong and linger needs revisiting.
type produceBatchHook struct{ m *Metrics }

// OnProduceBatchWritten implements kgo.HookProduceBatchWritten.
func (h *produceBatchHook) OnProduceBatchWritten(
	_ kgo.BrokerMetadata, topic string, _ int32, metrics kgo.ProduceBatchMetrics,
) {
	h.m.observeProduceBatch(topic, metrics.NumRecords)
}

var _ kgo.HookProduceBatchWritten = (*produceBatchHook)(nil)

// buildRecord validates everything that can be validated before any I/O and
// returns the record to produce.
//
// Order matters: the closed check comes first so a use-after-close is reported
// as such rather than as a confusing encoding error, and the key-kind check
// comes before encoding so a mis-keyed publish costs no marshal.
func (p *producer) buildRecord(t Topic, k recordKey, msg Message) (*kgo.Record, error) {
	if err := p.closed.err(); err != nil {
		return nil, err
	}
	if t.IsZero() {
		return nil, fmt.Errorf("%w: zero Topic", ErrInvalidTopic)
	}
	if err := k.validate(); err != nil {
		return nil, err
	}
	// The write-side half of the key guarantee. Unreachable through the
	// exported methods, which pair each topic with its domain type at compile
	// time; it exists so that adding a publish method wired to the wrong topic
	// fails loudly on its first call instead of quietly corrupting compaction.
	if t.KeyKind() != KeyKindUnknown && t.KeyKind() != k.kind {
		return nil, fmt.Errorf("%w: %s is keyed by %s, not %s",
			ErrWrongKeyKind, t.Name(), t.KeyKind(), k.kind)
	}

	// encode rejects a nil, empty or "null" payload with ErrEmptyPayload. That
	// is the guard that makes an accidental tombstone impossible: the only path
	// to a null value is the Tombstone API below.
	value, env, err := msg.encode(p.service, time.Now())
	if err != nil {
		return nil, err
	}
	return &kgo.Record{
		Topic:   t.Name(),
		Key:     []byte(k.id),
		Value:   value,
		Headers: env.headers(),
	}, nil
}

// recordBytes is the uncompressed on-the-wire size a record contributes.
func recordBytes(r *kgo.Record) int {
	n := len(r.Key) + len(r.Value)
	for _, h := range r.Headers {
		n += len(h.Key) + len(h.Value)
	}
	return n
}

// publish produces one record synchronously and returns the broker's verdict.
func (p *producer) publish(ctx context.Context, t Topic, k recordKey, msg Message) error {
	start := time.Now()

	r, err := p.buildRecord(t, k, msg)
	if err != nil {
		// A record that could not be built was NOT written, which is what
		// produce_records_total{outcome="error"} means. Counting it here is
		// what keeps the KafkaProduceErrors alert honest about a producer that
		// is failing on validation rather than on the network.
		p.m.observeProduce(t.Name(), 0, time.Since(start), err)
		return err
	}

	ctx, span := p.startProduceSpan(ctx, t, k, operationPublish, msg.Type, r)
	defer span.End()

	injectTrace(ctx, p.prop, r)

	res := p.cl.ProduceSync(ctx, r)
	err = res.FirstErr()
	p.observeProduced(t, r, time.Since(start), err)
	recordSpanError(span, err)
	if err != nil {
		return fmt.Errorf("kafka: produce to %s: %w", t.Name(), err)
	}
	span.SetAttributes(
		attribute.Int(attrMessagingPartition, int(r.Partition)),
		attribute.Int64(attrMessagingOffset, r.Offset),
	)
	return nil
}

// publishAsync buffers one record and returns immediately.
//
// # Errors are surfaced, never dropped
//
// The returned error covers everything decided BEFORE the record was buffered —
// a closed producer, a mis-keyed publish, an unmarshallable payload. Those are
// caller bugs and the caller learns about them on the spot.
//
// The delivery outcome arrives later, and there are exactly two ways it is
// surfaced and no third: done is called with it when done is non-nil, and it is
// logged at ERROR level with the topic, key and message type when done is nil.
// Either way produce_records_total{outcome="error"} and produce_errors_total
// increment. There is no configuration in which a delivery failure is silent —
// that is the difference between an asynchronous producer and a lossy one.
func (p *producer) publishAsync(ctx context.Context, t Topic, k recordKey, msg Message, done func(error)) error {
	start := time.Now()

	r, err := p.buildRecord(t, k, msg)
	if err != nil {
		p.m.observeProduce(t.Name(), 0, time.Since(start), err)
		return err
	}

	ctx, span := p.startProduceSpan(ctx, t, k, operationPublish, msg.Type, r)
	injectTrace(ctx, p.prop, r)

	p.inflight.Add(1)
	p.cl.Produce(ctx, r, func(rec *kgo.Record, perr error) {
		defer p.inflight.Done()
		defer span.End()

		p.observeProduced(t, rec, time.Since(start), perr)
		recordSpanError(span, perr)
		if perr == nil {
			span.SetAttributes(
				attribute.Int(attrMessagingPartition, int(rec.Partition)),
				attribute.Int64(attrMessagingOffset, rec.Offset),
			)
		} else if done == nil {
			p.log.Error("kafka async produce failed",
				slog.String("topic", rec.Topic),
				slog.String("key", string(rec.Key)),
				slog.String("type", msg.Type),
				slog.String("code", errorCode(perr)),
				slog.String("error", perr.Error()),
			)
		}
		if done != nil {
			if perr != nil {
				perr = fmt.Errorf("kafka: produce to %s: %w", rec.Topic, perr)
			}
			done(perr)
		}
	})
	return nil
}

// tombstone publishes a null value under key, permanently deleting it from a
// compacted topic's snapshot.
//
// Everything awkward about this path is deliberate; see the Tombstone type. The
// compaction check is the last of the three guards: a tombstone on a
// retention-based topic deletes nothing, it just writes a valueless record that
// every consumer must then decide how to ignore.
func (p *producer) tombstone(ctx context.Context, t Topic, k recordKey, ts Tombstone) error {
	start := time.Now()

	if err := p.closed.err(); err != nil {
		return err
	}
	if !t.Compacted() {
		// Two different situations, deliberately distinguished: a DECLARED
		// retention topic, where a null value provably deletes nothing and only
		// leaves a valueless record every consumer must learn to ignore; and a
		// topic outside the registry, whose cleanup policy this package simply
		// does not know. The conservative answer is the same, but an operator
		// reading the error deserves to know which one they hit.
		if t.Retention().Valid() {
			return fmt.Errorf("%w: %s is cleanup.policy=%s, so a null value deletes nothing there",
				ErrNotCompacted, t.Name(), t.Retention())
		}
		return fmt.Errorf("%w: %s is not a declared topic, so its cleanup.policy is unknown; "+
			"this package tombstones only topics topics.go declares compacted",
			ErrNotCompacted, t.Name())
	}
	if err := k.validate(); err != nil {
		return err
	}
	if t.KeyKind() != KeyKindUnknown && t.KeyKind() != k.kind {
		return fmt.Errorf("%w: %s is keyed by %s, not %s",
			ErrWrongKeyKind, t.Name(), t.KeyKind(), k.kind)
	}
	if err := ts.validate(); err != nil {
		return err
	}

	r := &kgo.Record{
		Topic:   t.Name(),
		Key:     []byte(k.id),
		Value:   nil, // THE null value. This is the deletion.
		Headers: ts.headers(p.service),
	}

	ctx, span := p.startProduceSpan(ctx, t, k, operationTombstone, "", r)
	defer span.End()
	span.SetAttributes(
		attribute.Bool(attrTombstone, true),
		attribute.String(attrTombstoneReason, ts.Reason),
	)

	injectTrace(ctx, p.prop, r)

	res := p.cl.ProduceSync(ctx, r)
	err := res.FirstErr()
	p.observeProduced(t, r, time.Since(start), err)
	recordSpanError(span, err)
	if err != nil {
		return fmt.Errorf("kafka: tombstone %s on %s: %w", k.id, t.Name(), err)
	}

	p.m.observeTombstone(t.Name())
	// WARN, not INFO. Every one of these removes a key from the snapshot every
	// client builds on connect, and after delete.retention.ms the tombstone is
	// itself collected and the deletion stops being visible in the log. This
	// line is the durable record that it was deliberate.
	p.log.Warn("kafka tombstone published; key deleted from the compacted snapshot",
		slog.String("topic", t.Name()),
		slog.String("key", k.id),
		slog.String("reason", ts.Reason),
		slog.Int("partition", int(r.Partition)),
		slog.Int64("offset", r.Offset),
	)
	return nil
}

// observeProduced records one completed produce and refreshes the buffered
// gauge.
func (p *producer) observeProduced(t Topic, r *kgo.Record, d time.Duration, err error) {
	p.m.observeProduce(t.Name(), recordBytes(r), d, err)
	p.m.setBufferedRecords(p.cl.BufferedProduceRecords())
}

// startProduceSpan opens the producer span. The span name follows the
// OpenTelemetry messaging convention of "<destination> <operation>", which is
// what makes Jaeger's operation list read as a list of topics.
func (p *producer) startProduceSpan(
	ctx context.Context, t Topic, k recordKey, operation, msgType string, r *kgo.Record,
) (context.Context, trace.Span) {
	attrs := baseSpanAttrs(t.Name(), operation)
	attrs = append(attrs,
		// The key is a market, event or wager identifier. It is a span
		// attribute (bounded per-trace) and never a metric label (unbounded
		// cardinality, and user-linked on wager.events).
		attribute.String(attrMessagingKey, k.id),
		attribute.Int(attrMessagingBodySize, len(r.Value)),
		attribute.Int(attrEnvelopeVersion, EnvelopeVersion),
	)
	if msgType != "" {
		attrs = append(attrs, attribute.String(attrMessageType, msgType))
	}
	return p.tracer.Start(ctx, t.Name()+" "+operation,
		trace.WithSpanKind(trace.SpanKindProducer),
		trace.WithAttributes(attrs...),
	)
}

// Flush blocks until every buffered record has been acknowledged or failed.
//
// It does NOT report per-record failures: those went to their promises (or to
// the error log) as they happened. What it returns is whether the flush itself
// completed inside its budget.
func (p *producer) Flush(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.flushTimeout)
	defer cancel()

	if err := p.cl.Flush(ctx); err != nil {
		return fmt.Errorf("kafka: flush producer: %w", err)
	}
	p.m.setBufferedRecords(p.cl.BufferedProduceRecords())
	return nil
}

// Close flushes, waits for outstanding async callbacks and closes the client.
//
// # Flush before close, and the ordering is the whole point
//
// kgo.Client.Close fails every still-buffered record. Closing without flushing
// first therefore DISCARDS whatever the process had accepted but not yet
// written — on wager.events that is a settlement entry that no poller will ever
// re-derive. So: refuse new publishes, drain, wait for the promises the drain
// released, and only then close.
//
// A flush that exceeds FlushTimeout is reported, and the close proceeds anyway.
// Blocking a shutdown for ever on an unreachable broker only converts a lost
// buffer into a SIGKILL that loses the same buffer with less to show for it.
//
// Close is idempotent and safe to call from a goroutine racing a publish.
func (p *producer) Close() error {
	if !p.closed.set() {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), p.flushTimeout)
	defer cancel()

	var flushErr error
	if err := p.cl.Flush(ctx); err != nil {
		flushErr = fmt.Errorf("kafka: flush on close (buffered records were dropped): %w", err)
		p.log.Error("kafka producer close: flush did not finish inside its budget",
			slog.Int64("buffered_records", p.cl.BufferedProduceRecords()),
			slog.String("error", err.Error()),
		)
	}

	p.cl.Close()
	// After Close, franz-go has settled every remaining promise. Waiting here
	// guarantees no callback is still touching the metrics or the logger when
	// Close returns, which is what makes a test's goroutine-leak check pass.
	p.inflight.Wait()
	p.m.setBufferedRecords(0)
	return flushErr
}

// -----------------------------------------------------------------------------
// OddsProducer — odds.raw.{provider}, odds.normalized, price.computed
// -----------------------------------------------------------------------------

// OddsProducer publishes on the low-latency posture.
//
// It has no method that writes wager.events, and that is a compile-time
// property rather than a convention. See doc.go for why the split is two types
// instead of one type with a knob.
type OddsProducer struct{ *producer }

// oddsProducerOpts is the low-latency half of doc.go's posture table.
func oddsProducerOpts() []kgo.Opt {
	return []kgo.Opt{
		// linger=0 is NOT "no batching". franz-go keeps buffering records for a
		// partition while a produce request for that partition is in flight, so
		// a high-rate publisher batches naturally with no fixed per-batch
		// delay. Linger only helps a LOW-rate publisher, and a low-rate
		// publisher does not need help. Checkable, not merely assertable:
		// sharpline_kafka_produce_batch_records must show batches above 1
		// under load.
		kgo.ProducerLinger(0),

		// Give up after 30s. A lost odds update is SELF-HEALING — the topic is
		// compacted and keyed by market, the next provider poll recomputes the
		// same market, and the next publish restores the current line. Better a
		// 30-second-old line than a producer buffer that grows until the
		// process dies. CLAUDE.md's headline SLO is staleness, and a record
		// still retrying after 30s has already blown it.
		kgo.RecordDeliveryTimeout(OddsRecordDeliveryTimeout),
		kgo.RecordRetries(OddsRecordRetries),
	}
}

// NewOddsProducer opens a producer for the odds topics.
//
// It performs a connectivity probe before returning unless
// ClientOptions.SkipStartupProbe is set, so a caller that gets a producer back
// has one that reached the cluster.
func NewOddsProducer(ctx context.Context, opts ProducerOptions) (*OddsProducer, error) {
	p, err := newProducer(ctx, opts, oddsProducerOpts())
	if err != nil {
		return nil, err
	}
	return &OddsProducer{producer: p}, nil
}

// PublishRaw publishes a provider payload to odds.raw.{provider}, keyed by
// event.
func (p *OddsProducer) PublishRaw(
	ctx context.Context, provider Provider, id domain.EventID, msg Message,
) error {
	t, err := OddsRaw(provider)
	if err != nil {
		return err
	}
	return p.publish(ctx, t, eventKey(id), msg)
}

// PublishRawAsync is PublishRaw without waiting for the acknowledgement. See
// publishAsync for exactly how a delivery failure is surfaced.
func (p *OddsProducer) PublishRawAsync(
	ctx context.Context, provider Provider, id domain.EventID, msg Message, done func(error),
) error {
	t, err := OddsRaw(provider)
	if err != nil {
		return err
	}
	return p.publishAsync(ctx, t, eventKey(id), msg, done)
}

// PublishNormalized publishes a normalized market to odds.normalized, keyed by
// market.
//
// The key type is what makes this safe:
//
//	p.PublishNormalized(ctx, someEventID, msg)  // does not compile
func (p *OddsProducer) PublishNormalized(ctx context.Context, id domain.MarketID, msg Message) error {
	return p.publish(ctx, OddsNormalized(), marketKey(id), msg)
}

// PublishNormalizedAsync is PublishNormalized without waiting for the
// acknowledgement.
func (p *OddsProducer) PublishNormalizedAsync(
	ctx context.Context, id domain.MarketID, msg Message, done func(error),
) error {
	return p.publishAsync(ctx, OddsNormalized(), marketKey(id), msg, done)
}

// PublishPrice publishes pricer output to price.computed, keyed by market.
func (p *OddsProducer) PublishPrice(ctx context.Context, id domain.MarketID, msg Message) error {
	return p.publish(ctx, PriceComputed(), marketKey(id), msg)
}

// PublishPriceAsync is PublishPrice without waiting for the acknowledgement.
func (p *OddsProducer) PublishPriceAsync(
	ctx context.Context, id domain.MarketID, msg Message, done func(error),
) error {
	return p.publishAsync(ctx, PriceComputed(), marketKey(id), msg, done)
}

// TombstoneNormalized permanently deletes a market from the odds.normalized
// snapshot.
//
// Read Tombstone before calling this. Every client that resyncs after the
// deletion stops seeing the market, and there is no undo short of republishing
// it.
func (p *OddsProducer) TombstoneNormalized(ctx context.Context, id domain.MarketID, ts Tombstone) error {
	return p.tombstone(ctx, OddsNormalized(), marketKey(id), ts)
}

// TombstonePrice permanently deletes a market from the price.computed snapshot.
func (p *OddsProducer) TombstonePrice(ctx context.Context, id domain.MarketID, ts Tombstone) error {
	return p.tombstone(ctx, PriceComputed(), marketKey(id), ts)
}

// -----------------------------------------------------------------------------
// AuditProducer — wager.events
// -----------------------------------------------------------------------------

// AuditProducer publishes the settlement audit trail.
//
// There is deliberately NO async method on this type and no tombstone method
// either. wager.events is retention-based, so a null value there deletes
// nothing; and a fire-and-forget audit entry is an audit entry whose loss the
// caller cannot detect, which defeats the purpose of the topic.
type AuditProducer struct{ *producer }

// auditProducerOpts is the never-give-up half of doc.go's posture table.
func auditProducerOpts() []kgo.Opt {
	return []kgo.Opt{
		// A few milliseconds of linger. Publishing here is synchronous and
		// low-rate, so there is no in-flight request to piggyback on the way
		// the odds path has; a small linger coalesces the events of one wager
		// into one batch and the caller never notices the delay.
		kgo.ProducerLinger(AuditLinger),

		// No RecordDeliveryTimeout and no RecordRetries: franz-go's defaults
		// are unlimited, and unlimited is what is wanted. A lost wager event is
		// recoverable by NOTHING — there is no poller that re-derives it, and
		// CLAUDE.md §4 makes the ledger the source of truth. Retrying for ever
		// with a synchronous Publish means the caller stays blocked and can
		// refuse to commit the surrounding database transaction, which is the
		// correct failure. The bound is the caller's context deadline, which is
		// where it belongs (CLAUDE.md §12: every external call has a timeout).
	}
}

// NewAuditProducer opens a producer for wager.events.
func NewAuditProducer(ctx context.Context, opts ProducerOptions) (*AuditProducer, error) {
	p, err := newProducer(ctx, opts, auditProducerOpts())
	if err != nil {
		return nil, err
	}
	return &AuditProducer{producer: p}, nil
}

// PublishWagerEvent publishes one settlement-audit event, keyed by wager, and
// blocks until the broker has acknowledged it on every in-sync replica.
//
// It is synchronous with no asynchronous sibling on purpose: the caller is
// expected to treat a non-nil return as a reason to fail the surrounding
// database transaction rather than to log and continue.
//
// # This is not exactly-once, and the gap is named rather than hidden
//
// The dangerous window is between COMMIT on Postgres and the ack from Kafka.
// Kafka transactions cannot span the two, so a transactional producer would
// convert a duplicate-message problem into a lost-message problem and call it
// progress. The correct mechanism is a transactional outbox — write the event
// to a Postgres table inside the same transaction as the ledger movement and
// relay it afterwards — and that is a phase 8 decision made with the betting
// and settlement packages. Until then, duplicates are expected and the
// idempotency key in Postgres is what absorbs them.
func (p *AuditProducer) PublishWagerEvent(ctx context.Context, id domain.WagerID, msg Message) error {
	return p.publish(ctx, WagerEvents(), wagerKey(id), msg)
}
