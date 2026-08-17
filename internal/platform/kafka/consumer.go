// The consume side: a consumer-group consumer with an explicit commit
// boundary, rebalance callbacks that distinguish REVOKED from LOST, group-lag
// export, and a snapshot reader for compacted topics.
//
// CLAUDE.md §10 says where the bugs are: "the interesting bugs live in
// consumer-group rebalancing and offset handling." Every non-obvious decision
// below is one of those bugs, written down as the reason it is not there.
//
// # The four things that make this at-least-once rather than approximately-once
//
//  1. AUTO-COMMIT IS OFF. franz-go's auto-commit commits the offsets of records
//     PollFetches has RETURNED, not records the application has PROCESSED.
//     A crash or a rebalance between the poll and the end of processing loses
//     work silently: the offsets say the records were handled and nobody will
//     ever fetch them again. kgo.AutoCommitMarks is a genuinely safe middle
//     ground and is still not used, because it hides WHEN the commit happens
//     (on a timer, in the background) and the whole point of this layer is that
//     the commit boundary is visible in the code that owns the work.
//
//  2. THE COMMIT IS OF HANDLED RECORDS, NOT POLLED ONES. The obvious revoke
//     callback is Client.CommitUncommittedOffsets and it is WRONG here:
//     "uncommitted" means "polled but not yet committed", which includes
//     records this process fetched and never handled. Committing those is data
//     loss with a clean conscience. So this file tracks the last SUCCESSFULLY
//     HANDLED record per partition and commits exactly those.
//
//  3. THE REBALANCE IS BLOCKED WHILE A BATCH IS IN FLIGHT
//     (kgo.BlockRebalanceOnPoll). No partition can be reassigned between the
//     poll and the commit that follows it. AllowRebalance is released from a
//     defer so that it runs on every path including a panicking handler —
//     forgetting it stalls the whole group until the session timeout expires.
//
//  4. REVOKED IS NOT LOST. On revoke the partitions are still ours for the
//     duration of the callback and franz-go blocks the rebalance until it
//     returns, so progress is committed synchronously. On LOST the session
//     already expired or the generation was fenced: the partitions belong to
//     another member, and a commit would either fail or succeed against a stale
//     generation and clobber that member's progress. Treating the two the same
//     is the classic duplicate-processing bug, so they are two callbacks with
//     two behaviours.
//
// # Concurrency: one record at a time, per member
//
// A poll's records are handled sequentially, in partition order. Kafka's unit of
// parallelism is the partition, and the way to go faster is more partitions and
// more group members — which is exactly what the phase-10 HPA demo scales.
// In-process fan-out would add a commit barrier, a partial-failure matrix and an
// ordering hazard in exchange for throughput a second replica provides for free.
package kafka

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Consumer geometry.
const (
	// DefaultSessionTimeout is how long the coordinator waits for a heartbeat
	// before declaring this member dead and reassigning its partitions. Matches
	// franz-go's default; stated here because it is the number that turns a
	// slow handler into a LOST partition.
	DefaultSessionTimeout = 45 * time.Second

	// DefaultRebalanceTimeout bounds how long the group waits for this member
	// to finish its in-flight batch and rejoin. With BlockRebalanceOnPoll, a
	// batch that takes longer than this gets the member fenced — its partitions
	// are LOST rather than revoked, and its work is redone by whoever picks
	// them up. MaxPollRecords is the lever that keeps a batch under it.
	DefaultRebalanceTimeout = 60 * time.Second

	// DefaultHeartbeatInterval is franz-go's default: roughly a third of the
	// session timeout, so two missed heartbeats are survivable.
	DefaultHeartbeatInterval = 3 * time.Second

	// DefaultMaxPollRecords bounds one processing batch.
	//
	// It is a REBALANCE-SAFETY parameter, not a throughput one. Because the
	// rebalance is blocked from the poll until the commit, the worst-case block
	// is MaxPollRecords × handler duration, and exceeding RebalanceTimeout
	// converts a clean revoke into a fenced generation. 500 records at the 250ms
	// handler-latency alert threshold is well inside the 60s budget even in the
	// pathological case.
	DefaultMaxPollRecords = 500

	// DefaultCommitTimeout bounds one synchronous offset commit. The commit
	// blocks the consume loop, so it is part of the pipeline's latency budget
	// and not merely a diagnostic.
	DefaultCommitTimeout = 10 * time.Second

	// DefaultLagRefreshInterval matches deploy/observability/prometheus.yml's
	// scrape_interval (15s). Refreshing faster would export values Prometheus
	// never reads while paying for a DescribeGroups + OffsetFetch +
	// ListEndOffsets round trip each time; refreshing slower would make the
	// lag gauges lag.
	DefaultLagRefreshInterval = 15 * time.Second

	// lagRefreshTimeout bounds one lag refresh.
	//
	// It is a fixed value rather than the refresh interval, and it is allowed
	// to exceed it. Refreshes cannot overlap — the refresher calls this
	// synchronously from its own goroutine, so a slow round trip delays the
	// next tick instead of racing it — and deriving the budget from the
	// interval made a short interval silently unable to complete a three-request
	// admin call, which presented as permanently absent lag gauges rather than
	// as an error.
	lagRefreshTimeout = 10 * time.Second

	// DefaultSnapshotTimeout bounds a whole snapshot read. Deliberately longer
	// than any single request: it is the budget for reading a compacted topic
	// from the beginning, and it is the number a Kubernetes startupProbe has to
	// be sized against.
	DefaultSnapshotTimeout = 60 * time.Second
)

// commit reasons, used as a log field so the three commit sites are
// distinguishable in a merged log stream.
const (
	commitReasonBatch    = "batch"
	commitReasonRevoke   = "revoke"
	commitReasonShutdown = "shutdown"
)

// -----------------------------------------------------------------------------
// Handler
// -----------------------------------------------------------------------------

// Handler processes one delivered record.
//
// It is declared here rather than alongside any implementation because this
// package is the CONSUMER of the abstraction (CLAUDE.md §12: "Interfaces are
// declared by the consumer, not the producer. Keep them small.").
//
// # The contract a handler must honour
//
//   - IT MUST BE IDEMPOTENT. Delivery is at-least-once, so a redelivery after a
//     crash or a rebalance is normal operation and not an error. doc.go
//     enumerates how each topic achieves this: compacted topics are
//     last-write-wins by key, price inserts collide on the prices natural-key
//     index, and wager.events relies on an idempotency key in Postgres.
//   - IT MUST HANDLE A TOMBSTONE. On a compacted topic Delivery.Tombstone means
//     the key is gone for ever; ignoring it leaves a deleted market in whatever
//     cache or table the handler maintains, permanently, because no further
//     record for that key is coming.
//   - IT MUST RETURN PROMPTLY. The rebalance is blocked for the whole batch.
//   - IT MUST NOT RETAIN Delivery.Value() past its return: those bytes alias the
//     fetch buffer.
//
// Returning an error does not skip the record. Under the default
// ErrorPolicyStop it stops the consumer with the offset uncommitted, so the
// record is redelivered — a poison record retries for ever rather than being
// silently dropped, and that visible failure is the intended one.
type Handler interface {
	HandleMessage(ctx context.Context, d *Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d *Delivery) error

// HandleMessage implements Handler.
func (f HandlerFunc) HandleMessage(ctx context.Context, d *Delivery) error { return f(ctx, d) }

// -----------------------------------------------------------------------------
// Error policy
// -----------------------------------------------------------------------------

// ErrorPolicy decides what a failing record does to the consume loop.
type ErrorPolicy uint8

const (
	// ErrorPolicyStop stops the consumer on the first handler or decode
	// failure, leaving that record's offset uncommitted so it is redelivered.
	// It is the ZERO VALUE, and therefore the default, on purpose: the
	// alternative default silently drops data, and a silent default that loses
	// records is not a default anybody would choose deliberately.
	//
	// Records handled successfully BEFORE the failure are still committed, so
	// stopping costs no rework beyond the failing record itself.
	ErrorPolicyStop ErrorPolicy = iota

	// ErrorPolicySkip logs the failure at ERROR, counts it, and advances past
	// the record.
	//
	// It is correct for exactly one situation: a stream where a single
	// unprocessable record must not halt the pipeline and the loss is
	// acceptable — the odds path, where the next provider poll republishes the
	// same market. It is INDEFENSIBLE on wager.events, where a skipped record
	// is a missing audit entry that nothing will re-derive.
	ErrorPolicySkip
)

// String implements fmt.Stringer.
func (p ErrorPolicy) String() string {
	switch p {
	case ErrorPolicyStop:
		return "stop"
	case ErrorPolicySkip:
		return "skip"
	default:
		return "unknown"
	}
}

// Valid reports whether p is a policy this package implements.
func (p ErrorPolicy) Valid() bool { return p == ErrorPolicyStop || p == ErrorPolicySkip }

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// ConsumerOptions configures a consumer-group consumer.
type ConsumerOptions struct {
	ClientOptions

	// Group is the consumer group id. Required.
	//
	// It is the unit of offset ownership: two processes sharing a group split
	// the partitions between them, two processes with different groups each get
	// every record. Changing it on an existing deployment starts from
	// StartAtEnd's answer with no committed offsets, which on a compacted topic
	// means replaying the entire snapshot.
	Group string

	// Topics is the subscription list. Required, non-empty.
	Topics []string

	// ErrorPolicy decides what a failing record does. Zero value is
	// ErrorPolicyStop.
	ErrorPolicy ErrorPolicy

	// StartAtEnd makes a group with NO COMMITTED OFFSETS begin at the end of
	// the log instead of the beginning. It has no effect once the group has
	// committed anything.
	//
	// The default (beginning) is the safe one and is required for correctness
	// on a compacted topic: starting at the end means the consumer never sees
	// the markets that existed before it joined, which is precisely the
	// snapshot the compacted log exists to provide (CLAUDE.md §3). Set it only
	// for a consumer that genuinely wants live-only data and can tolerate
	// missing everything that happened before it started.
	StartAtEnd bool

	// InstanceID enables static group membership.
	//
	// A member with an instance id keeps its partitions across a restart
	// instead of triggering a rebalance, which is attractive for a rolling
	// update. It is off by default because of the cost: franz-go does NOT send
	// a leave-group request for a static member, so a member that goes away for
	// good holds its partitions until SessionTimeout expires, and during that
	// window nothing consumes them. Set it only with a session timeout sized
	// against the deployment's restart time.
	InstanceID string

	// Timeouts and budgets. Zero means the corresponding Default* constant.
	SessionTimeout     time.Duration
	RebalanceTimeout   time.Duration
	HeartbeatInterval  time.Duration
	CommitTimeout      time.Duration
	LagRefreshInterval time.Duration
	MaxPollRecords     int

	// DisableLagExport turns off the background group-lag refresher.
	//
	// The refresher costs one DescribeGroups + OffsetFetch + ListEndOffsets per
	// interval and it populates sharpline_kafka_consumer_lag_records and
	// sharpline_kafka_consumer_lag_seconds, which are on the Grafana dashboard
	// and behind two alert rules. Turning it off is for a one-shot job that
	// serves no /metrics endpoint, never for a long-running service.
	DisableLagExport bool
}

func (o ConsumerOptions) validate() error {
	if err := o.ClientOptions.validate(); err != nil {
		return err
	}
	switch {
	case o.Group == "":
		return fmt.Errorf("%w: Group is empty", ErrInvalidOptions)
	case len(o.Topics) == 0:
		return fmt.Errorf("%w: Topics is empty", ErrInvalidOptions)
	case !o.ErrorPolicy.Valid():
		return fmt.Errorf("%w: ErrorPolicy %d is not a policy this package implements",
			ErrInvalidOptions, o.ErrorPolicy)
	}
	for i, t := range o.Topics {
		// Checked here rather than left to the broker: with auto-creation
		// disabled (CLAUDE.md §9) a typo would otherwise surface as an
		// UNKNOWN_TOPIC_OR_PARTITION several seconds into a poll loop, attached
		// to no particular line of code.
		if err := validateTopicName(t); err != nil {
			return fmt.Errorf("%w: Topics[%d]: %w", ErrInvalidOptions, i, err)
		}
	}
	return nil
}

// -----------------------------------------------------------------------------
// Per-partition bookkeeping
// -----------------------------------------------------------------------------

// topicPartition identifies one partition. A comparable struct rather than a
// nested map, because every operation here is a whole-key lookup.
type topicPartition struct {
	topic     string
	partition int32
}

// partitionState is what the commit boundary and the lag gauges need to know
// about one partition this member owns.
type partitionState struct {
	// pending is the last record handled SUCCESSFULLY and not yet committed.
	// Nil once committed. This — never "polled" — is what gets committed.
	pending *kgo.Record

	// committed reports whether this member has committed anything on this
	// partition. sharpline_kafka_consumer_lag_seconds stays ABSENT until it is
	// true: a gap in the graph is honest, a fabricated zero is not.
	committed bool

	// committedTime is the record timestamp of the newest record committed on
	// this partition, which is what makes lag_seconds a wall-clock age rather
	// than a record count.
	committedTime time.Time
}

// -----------------------------------------------------------------------------
// Consumer
// -----------------------------------------------------------------------------

// Consumer is a consumer-group member.
//
// One Consumer runs one loop. Scaling is more group members (more replicas),
// not more loops in a process — see the concurrency note at the top of the file.
type Consumer struct {
	healthChecker

	cl  *kgo.Client
	adm *kadm.Client

	group       string
	topics      []string
	errorPolicy ErrorPolicy

	commitTimeout  time.Duration
	lagInterval    time.Duration
	maxPollRecords int
	exportLag      bool

	log    *slog.Logger
	m      *Metrics
	tracer trace.Tracer
	prop   propagation.TextMapPropagator

	// mu guards state and assigned. The rebalance callbacks run on franz-go's
	// group goroutine while Run owns the loop, so these are genuinely shared
	// even though BlockRebalanceOnPoll keeps the interesting windows disjoint.
	mu       sync.Mutex
	state    map[topicPartition]*partitionState
	assigned map[string]map[int32]struct{}

	running atomic.Bool
	closed  closedFlag
}

// NewConsumer opens a consumer-group member and proves it can reach the
// cluster.
//
// It does not join the group and does not consume anything until Run is called.
func NewConsumer(ctx context.Context, opts ConsumerOptions) (*Consumer, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	m, err := opts.resolveMetrics()
	if err != nil {
		return nil, err
	}

	log := opts.Logger.With(
		slog.String("component", "kafka.consumer"),
		slog.String("group", opts.Group),
	)

	c := &Consumer{
		group:          opts.Group,
		topics:         append([]string(nil), opts.Topics...),
		errorPolicy:    opts.ErrorPolicy,
		commitTimeout:  positiveOr(opts.CommitTimeout, DefaultCommitTimeout),
		lagInterval:    positiveOr(opts.LagRefreshInterval, DefaultLagRefreshInterval),
		maxPollRecords: positiveIntOr(opts.MaxPollRecords, DefaultMaxPollRecords),
		exportLag:      !opts.DisableLagExport,
		log:            log,
		m:              m,
		tracer:         opts.tracer(),
		prop:           opts.propagator(),
		state:          make(map[topicPartition]*partitionState),
		assigned:       make(map[string]map[int32]struct{}),
	}

	reset := kgo.NewOffset().AtStart()
	if opts.StartAtEnd {
		reset = kgo.NewOffset().AtEnd()
	}

	kopts := opts.baseOpts()
	kopts = append(kopts,
		kgo.ConsumerGroup(opts.Group),
		kgo.ConsumeTopics(c.topics...),
		kgo.ConsumeResetOffset(reset),

		// The two options this file is built around. See the header comment.
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),

		// Cooperative-sticky, set explicitly rather than relied on as franz-go's
		// default. Eager balancers revoke EVERY partition from EVERY member on
		// any membership change; cooperative revokes only what actually moves.
		// That matters here specifically because phase 10's HPA scales the
		// consumer pool up and down under load, so membership changes are
		// routine rather than exceptional.
		//
		// The cost, stated because it will bite someone during a rolling
		// upgrade: a group cannot mix cooperative and eager members. Changing
		// this value is a stop-the-group migration, not a config tweak.
		kgo.Balancers(kgo.CooperativeStickyBalancer()),

		kgo.SessionTimeout(positiveOr(opts.SessionTimeout, DefaultSessionTimeout)),
		kgo.RebalanceTimeout(positiveOr(opts.RebalanceTimeout, DefaultRebalanceTimeout)),
		kgo.HeartbeatInterval(positiveOr(opts.HeartbeatInterval, DefaultHeartbeatInterval)),

		kgo.OnPartitionsAssigned(c.onAssigned),
		kgo.OnPartitionsRevoked(c.onRevoked),
		kgo.OnPartitionsLost(c.onLost),
	)
	if opts.InstanceID != "" {
		kopts = append(kopts, kgo.InstanceID(opts.InstanceID))
	}

	cl, err := kgo.NewClient(kopts...)
	if err != nil {
		return nil, fmt.Errorf("%w: build consumer client: %w", ErrInvalidOptions, err)
	}
	c.cl = cl
	c.adm = kadm.NewClient(cl)
	c.healthChecker = healthChecker{
		cl:      cl,
		m:       m,
		log:     log,
		timeout: positiveOr(opts.ProbeTimeout, DefaultProbeTimeout),
		closed:  &c.closed,
	}

	if !opts.SkipStartupProbe {
		if err := awaitReady(ctx, cl, opts.ClientOptions, m, log); err != nil {
			cl.Close()
			return nil, err
		}
	}
	return c, nil
}

// Group returns the consumer group id.
func (c *Consumer) Group() string { return c.group }

// Topics returns the subscription list.
func (c *Consumer) Topics() []string { return append([]string(nil), c.topics...) }

// Run joins the group and consumes until ctx is cancelled, Close is called, or
// a record fails under ErrorPolicyStop.
//
// It returns nil for a clean stop — a cancelled context or a closed consumer —
// and the failure otherwise. It blocks, so a cmd/ entrypoint runs it in a
// goroutine alongside the httpx server and cancels the shared context on
// SIGTERM.
//
// Only one Run may be in flight per Consumer.
func (c *Consumer) Run(ctx context.Context, h Handler) error {
	if h == nil {
		return fmt.Errorf("%w: Handler is nil", ErrInvalidOptions)
	}
	if err := c.closed.err(); err != nil {
		return err
	}
	if !c.running.CompareAndSwap(false, true) {
		return fmt.Errorf("kafka: consumer for group %q is already running", c.group)
	}
	defer c.running.Store(false)

	if c.exportLag {
		lagCtx, stopLag := context.WithCancel(ctx)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.runLagRefresher(lagCtx)
		}()
		defer func() {
			stopLag()
			wg.Wait()
		}()
	}

	c.log.Info("kafka consumer running",
		slog.Any("topics", c.topics),
		slog.String("error_policy", c.errorPolicy.String()),
		slog.Int("max_poll_records", c.maxPollRecords),
	)

	for {
		if ctx.Err() != nil || c.closed.isSet() {
			return c.stop(ctx)
		}

		// From here until AllowRebalance (deferred inside handleFetches) no
		// rebalance can be processed.
		fetches := c.cl.PollRecords(ctx, c.maxPollRecords)

		if fetches.IsClientClosed() {
			return c.stop(ctx)
		}
		if err := c.handleFetches(ctx, fetches, h); err != nil {
			// A context cancellation surfaced through a fetch is a clean stop,
			// not a failure: it is how Run is asked to return.
			if ctx.Err() != nil && errors.Is(err, context.Canceled) {
				return c.stop(ctx)
			}
			c.log.Error("kafka consumer stopping on error",
				slog.String("error_policy", c.errorPolicy.String()),
				slog.String("error", err.Error()),
			)
			// Commit whatever was handled before the failure. The failing
			// record itself is NOT committed and will be redelivered.
			_ = c.commitPending(ctx, commitReasonShutdown)
			return err
		}
	}
}

// stop performs the final commit and reports a clean exit.
func (c *Consumer) stop(ctx context.Context) error {
	if err := c.commitPending(ctx, commitReasonShutdown); err != nil {
		// Already logged and counted. Returned as nil regardless: the loop
		// stopped for a legitimate reason, and a failed final commit means
		// redelivery, which at-least-once already promises.
		c.log.Warn("kafka consumer stopped with an uncommitted batch; it will be redelivered")
	}
	c.log.Info("kafka consumer stopped")
	return nil
}

// handleFetches processes one poll and commits exactly what it handled.
func (c *Consumer) handleFetches(ctx context.Context, fetches kgo.Fetches, h Handler) error {
	// MANDATORY, ON EVERY PATH. Until this runs the group cannot rebalance —
	// not to add a member, not to remove one, not to recover a dead one — and
	// every other member sits in the join phase until this member's session
	// timeout expires. Deferred rather than called at the end so that a
	// panicking handler cannot wedge the group.
	defer c.cl.AllowRebalance()

	c.observeFetchErrors(fetches)

	var handlerErr error
	fetches.EachPartition(func(ftp kgo.FetchTopicPartition) {
		if handlerErr != nil {
			return
		}
		for _, rec := range ftp.Records {
			if err := c.handleRecord(ctx, h, rec); err != nil {
				handlerErr = err
				return
			}
		}
	})

	// Commit before returning the handler error, so the successfully handled
	// prefix of the batch is not redone.
	//
	// A commit failure is deliberately NOT fatal: it has already been logged
	// and counted (offset_commits_total{outcome="error"}, which is what the
	// KafkaOffsetCommitErrors alert watches), the pending records are retained,
	// and the next round's commit covers them. Killing the consumer over a
	// coordinator that moved would turn a self-healing condition into an
	// outage.
	_ = c.commitPending(ctx, commitReasonBatch)

	return handlerErr
}

// observeFetchErrors records per-partition fetch failures.
//
// None of them stop the loop. franz-go retries fetch errors internally, so the
// honest signal is a RATE rather than an occurrence — which is exactly what
// sharpline_kafka_fetch_errors_total is for: a sustained rate means it is
// retrying and not succeeding.
func (c *Consumer) observeFetchErrors(fetches kgo.Fetches) {
	for _, fe := range fetches.Errors() {
		if errors.Is(fe.Err, context.Canceled) || errors.Is(fe.Err, kgo.ErrClientClosed) {
			continue
		}
		c.m.observeFetchError(c.group, fe.Topic, fe.Err)
		c.log.Warn("kafka fetch error",
			slog.String("topic", fe.Topic),
			slog.Int("partition", int(fe.Partition)),
			slog.String("code", errorCode(fe.Err)),
			slog.String("error", fe.Err.Error()),
		)
	}
}

// handleRecord decodes one record, hands it to the handler, and marks it
// pending on success.
//
// A nil return means "advance past this record", which is true both for a
// success and for a failure the policy says to skip.
func (c *Consumer) handleRecord(ctx context.Context, h Handler, rec *kgo.Record) error {
	d, err := newDelivery(rec)
	if err != nil {
		c.m.observeDecodeError(c.group, rec.Topic, err)
		c.log.Error("kafka record could not be decoded",
			slog.String("topic", rec.Topic),
			slog.Int("partition", int(rec.Partition)),
			slog.Int64("offset", rec.Offset),
			slog.String("key", string(rec.Key)),
			slog.String("reason", decodeFailureReason(err)),
			slog.String("error", err.Error()),
		)
		if c.errorPolicy == ErrorPolicyStop {
			return fmt.Errorf("kafka: group %s: %w", c.group, err)
		}
		// Skipping is the only way past an undecodable record: retrying it
		// cannot change the bytes on disk, so under ErrorPolicySkip it is
		// counted, logged and advanced over.
		c.mark(rec)
		return nil
	}

	c.m.observeConsumed(c.group, rec.Topic, recordBytes(rec))

	// The consumer span is a CHILD of the producer's, joined through the record
	// headers, which is what makes CLAUDE.md §9's "ingest → pricer → stream" a
	// single trace rather than four disconnected ones.
	hctx := extractTrace(ctx, c.prop, rec)
	hctx, span := c.tracer.Start(hctx, rec.Topic+" "+operationProcess,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(c.consumeSpanAttrs(d)...),
	)

	start := time.Now()
	herr := h.HandleMessage(hctx, d)
	elapsed := time.Since(start)

	recordSpanError(span, herr)
	span.End()
	c.m.observeHandler(c.group, rec.Topic, elapsed, herr)

	if herr != nil {
		c.log.Error("kafka message handler failed",
			slog.Any("record", d),
			slog.String("duration", elapsed.String()),
			slog.String("error", herr.Error()),
		)
		if c.errorPolicy == ErrorPolicyStop {
			return fmt.Errorf("%w: %s/%d offset %d: %w",
				ErrHandlerFailed, rec.Topic, rec.Partition, rec.Offset, herr)
		}
	}

	c.mark(rec)
	return nil
}

// consumeSpanAttrs builds the per-record span attributes.
func (c *Consumer) consumeSpanAttrs(d *Delivery) []attribute.KeyValue {
	attrs := baseSpanAttrs(d.Topic, operationProcess)
	attrs = append(attrs,
		attribute.String(attrMessagingGroup, c.group),
		attribute.String(attrMessagingKey, d.Key),
		attribute.Int(attrMessagingPartition, int(d.Partition)),
		attribute.Int64(attrMessagingOffset, d.Offset),
		attribute.Int(attrMessagingBodySize, len(d.Value())),
	)
	if d.Tombstone {
		attrs = append(attrs,
			attribute.Bool(attrTombstone, true),
			attribute.String(attrTombstoneReason, d.TombstoneReason),
		)
		return attrs
	}
	return append(attrs,
		attribute.String(attrMessageType, d.Envelope.Type),
		attribute.Int(attrEnvelopeVersion, d.Envelope.Version),
	)
}

// -----------------------------------------------------------------------------
// Offset tracking and committing
// -----------------------------------------------------------------------------

// mark records rec as the newest successfully handled record on its partition.
func (c *Consumer) mark(rec *kgo.Record) {
	tp := topicPartition{topic: rec.Topic, partition: rec.Partition}

	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.state[tp]
	if st == nil {
		st = &partitionState{}
		c.state[tp] = st
	}
	// Records arrive in offset order within a partition, but a rewound commit
	// is destructive enough (it replays everything between) to be worth one
	// comparison.
	if st.pending == nil || rec.Offset > st.pending.Offset {
		st.pending = rec
	}
}

// pendingRecords returns the newest handled-but-uncommitted record for every
// partition, sorted so the log line and the commit request are deterministic.
func (c *Consumer) pendingRecords() []*kgo.Record {
	c.mu.Lock()
	defer c.mu.Unlock()

	recs := make([]*kgo.Record, 0, len(c.state))
	for _, st := range c.state {
		if st.pending != nil {
			recs = append(recs, st.pending)
		}
	}
	sort.Slice(recs, func(i, j int) bool {
		if recs[i].Topic != recs[j].Topic {
			return recs[i].Topic < recs[j].Topic
		}
		return recs[i].Partition < recs[j].Partition
	})
	return recs
}

// confirmCommitted clears the pending records a commit succeeded for and stamps
// what the lag gauges need.
func (c *Consumer) confirmCommitted(recs []*kgo.Record) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, rec := range recs {
		st := c.state[topicPartition{topic: rec.Topic, partition: rec.Partition}]
		if st == nil {
			// The partition was revoked or lost between the commit and here.
			continue
		}
		st.committed = true
		st.committedTime = rec.Timestamp
		if st.pending != nil && st.pending.Offset <= rec.Offset {
			st.pending = nil
		}
	}
}

// commitPending commits the newest successfully handled record on every
// partition.
//
// # The commit deliberately outlives its caller's cancellation
//
// By the time this runs the work IS done. Abandoning the commit because ctx was
// cancelled guarantees the next member redoes it, which is the exact failure
// at-least-once is supposed to make rare rather than routine. So the caller's
// cancellation is dropped (context.WithoutCancel keeps the values, including
// trace context) and CommitTimeout is what actually bounds it — a shutdown
// therefore cannot hang for ever either.
func (c *Consumer) commitPending(ctx context.Context, reason string) error {
	recs := c.pendingRecords()
	if len(recs) == 0 {
		return nil
	}

	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.commitTimeout)
	defer cancel()

	start := time.Now()
	// CommitRecords commits max(offset)+1 per partition and goes through
	// CommitOffsetsSync internally, so it cannot be cancelled by a concurrent
	// commit or by a rebalance — which is what makes it correct inside
	// OnPartitionsRevoked as well as in the loop.
	err := c.cl.CommitRecords(cctx, recs...)
	c.m.observeCommit(c.group, time.Since(start), err)

	if err != nil {
		c.log.Error("kafka offset commit failed; these records will be redelivered",
			slog.String("reason", reason),
			slog.Int("partitions", len(recs)),
			slog.String("code", errorCode(err)),
			slog.String("error", err.Error()),
		)
		return fmt.Errorf("kafka: commit offsets for group %s: %w", c.group, err)
	}

	c.confirmCommitted(recs)
	c.log.Debug("kafka offsets committed",
		slog.String("reason", reason),
		slog.Int("partitions", len(recs)),
	)
	return nil
}

// -----------------------------------------------------------------------------
// Rebalance callbacks
// -----------------------------------------------------------------------------

// onAssigned runs when partitions are added to this member's assignment.
//
// With a cooperative balancer this carries only the NEWLY added partitions, so
// the gauge is recomputed from the running set rather than set to what arrived.
func (c *Consumer) onAssigned(_ context.Context, _ *kgo.Client, added map[string][]int32) {
	c.mu.Lock()
	for topic, parts := range added {
		ps := c.assigned[topic]
		if ps == nil {
			ps = make(map[int32]struct{}, len(parts))
			c.assigned[topic] = ps
		}
		for _, p := range parts {
			ps[p] = struct{}{}
		}
	}
	counts := c.assignedCountsLocked()
	c.mu.Unlock()

	// Counted here, once, and NOT also in the revoke and lost callbacks:
	// a rebalance that revokes and then assigns would otherwise increment
	// sharpline_kafka_consumer_group_rebalances_total two or three times and
	// the KafkaRebalanceStorm alert would fire on a stable group.
	c.m.observeRebalance(c.group)
	for topic, n := range counts {
		c.m.setAssigned(c.group, topic, n)
	}

	c.log.Info("kafka partitions assigned",
		slog.Any("added", added),
		slog.Any("owned", counts),
	)
}

// onRevoked runs when partitions are being taken away in an orderly rebalance.
//
// franz-go BLOCKS the rebalance until this returns. That is what makes the
// commit here safe and what makes it worth doing: the partitions are still
// ours, the generation is still current, and the member that receives them next
// starts from work this member actually finished.
func (c *Consumer) onRevoked(ctx context.Context, _ *kgo.Client, revoked map[string][]int32) {
	// Commit FIRST, while the partitions are still owned. commitPending commits
	// every pending partition, not just the revoked ones — legal, because this
	// member owns them all in this generation, and it avoids losing progress on
	// a partition revoked later in the same rebalance.
	if err := c.commitPending(ctx, commitReasonRevoke); err != nil {
		// Already logged and counted inside commitPending. Nothing further can
		// be done: the partitions are moving regardless, and the new owner will
		// redeliver from the last successful commit.
		c.log.Warn("kafka revoke completed without committing; records will be redelivered",
			slog.Any("revoked", revoked))
	}

	for topic, parts := range revoked {
		c.m.observeRevoked(c.group, topic, len(parts))
	}
	c.forget(revoked)

	c.log.Info("kafka partitions revoked", slog.Any("revoked", revoked))
}

// onLost runs when partitions were taken away WITHOUT this member being asked —
// the session expired, or the generation was fenced.
//
// NOTHING IS COMMITTED HERE, and that is the whole difference from onRevoked.
// The partitions already belong to another member. A commit against a stale
// generation either fails outright or, if the coordinator accepts it, moves the
// group's offset backwards over work the new owner has already done — silently
// duplicating or skipping records depending on which way it lands. Treating
// LOST as REVOKED is the classic bug this callback exists to not have.
//
// At-least-once covers the gap: whatever this member handled and did not commit
// is redelivered to the new owner.
func (c *Consumer) onLost(_ context.Context, _ *kgo.Client, lost map[string][]int32) {
	for topic, parts := range lost {
		c.m.observeLost(c.group, topic, len(parts))
	}
	c.forget(lost)

	// WARN: non-zero means work was redone, and it usually means a handler
	// exceeded the rebalance timeout.
	c.log.Warn("kafka partitions LOST; uncommitted work on them will be redelivered to the new owner",
		slog.Any("lost", lost))
}

// forget drops this member's tracking for partitions it no longer owns and
// deletes their lag series.
//
// Deleting the series matters more than it looks: a gauge left behind keeps its
// last value for ever, so the dashboard's `sum by (group, topic)` would
// double-count the partition — once from the member that lost it and once from
// the member that gained it. A stale gauge is worse than a missing one because
// it is indistinguishable from a real measurement.
func (c *Consumer) forget(partitions map[string][]int32) {
	c.mu.Lock()
	for topic, parts := range partitions {
		for _, p := range parts {
			delete(c.state, topicPartition{topic: topic, partition: p})
			if ps := c.assigned[topic]; ps != nil {
				delete(ps, p)
			}
		}
	}
	counts := c.assignedCountsLocked()
	c.mu.Unlock()

	for topic, parts := range partitions {
		for _, p := range parts {
			c.m.forgetPartitionLag(c.group, topic, strconv.Itoa(int(p)))
		}
	}
	for topic, n := range counts {
		c.m.setAssigned(c.group, topic, n)
	}
}

// assignedCountsLocked returns the partition count per topic. Topics are never
// removed from the map, so a topic that drops to zero partitions still reports
// 0 rather than leaving its gauge frozen at its last non-zero value.
func (c *Consumer) assignedCountsLocked() map[string]int {
	counts := make(map[string]int, len(c.assigned))
	for topic, ps := range c.assigned {
		counts[topic] = len(ps)
	}
	return counts
}

// -----------------------------------------------------------------------------
// Group lag
// -----------------------------------------------------------------------------

// runLagRefresher periodically republishes the two lag gauges.
//
// The first refresh is immediate rather than one interval late: a member that
// has just restarted otherwise leaves a hole in the lag graph for exactly the
// window an operator is most likely to be staring at it.
func (c *Consumer) runLagRefresher(ctx context.Context) {
	t := time.NewTicker(c.lagInterval)
	defer t.Stop()

	c.refreshLag(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refreshLag(ctx)
		}
	}
}

// refreshLag reads group lag from the coordinator and exports the partitions
// THIS MEMBER owns.
//
// # Why it is filtered to owned partitions
//
// Group lag is readable cluster-wide from any member, so a consumer that
// exported all of it would multiply the graph by the member count. The Grafana
// panel aggregates with `sum by (group, topic)`, and that sum equals the true
// group lag only if each partition is reported exactly once. Filtering to the
// owned set is what makes that true — and it is also what makes
// forgetPartitionLag necessary on revoke.
//
// # Why it is a poll and not derived from the fetches
//
// FetchPartition.HighWatermark arrives free with every fetch, but franz-go only
// surfaces a fetch that HAS errors or records: a partition that is caught up,
// or one that is idle because the slate is quiet, never appears. Deriving lag
// from fetches alone would therefore report nothing for exactly the partitions
// an operator most wants confirmation about, and would go stale precisely when
// a handler stalls — the case the alert exists for.
func (c *Consumer) refreshLag(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, lagRefreshTimeout)
	defer cancel()

	lags, err := c.adm.Lag(ctx, c.group)
	if err != nil {
		c.noteLagRefreshFailure(err)
		return
	}
	described, ok := lags[c.group]
	if !ok {
		c.noteLagRefreshFailure(fmt.Errorf("coordinator returned no lag for group %q", c.group))
		return
	}
	if err := described.Error(); err != nil {
		c.noteLagRefreshFailure(err)
		return
	}

	owned := c.ownedStates()
	now := time.Now()

	for topic, parts := range described.Lag {
		for part, lag := range parts {
			st, mine := owned[topicPartition{topic: topic, partition: part}]
			if !mine {
				continue
			}
			if lag.Err != nil || lag.Lag < 0 {
				// A partition whose end offset could not be listed leaves its
				// gauges at their previous value rather than at a fabricated
				// one. The counter is how that staleness is detectable.
				c.m.observeLagRefreshError(c.group)
				continue
			}

			ps := strconv.Itoa(int(part))
			c.m.setLagRecords(c.group, topic, ps, float64(lag.Lag))

			// lag_seconds stays ABSENT until this member has committed
			// something on the partition: with no committed record there is no
			// instant to measure an age from, and reporting 0 would claim
			// perfect freshness for a partition nothing has processed.
			if !st.committed {
				continue
			}
			if lag.Lag == 0 {
				c.m.setLagSeconds(c.group, topic, ps, 0)
				continue
			}
			// Clamped at zero. committedTime is the RECORD's timestamp, which
			// records are produced with CreateTime semantics — so it is the
			// PRODUCER's clock, not this process's. Any skew between the two
			// hosts makes the subtraction negative, and a negative age on a
			// gauge whose alert is `> 30` is worse than a wrong one: it is
			// silently unfalsifiable.
			age := now.Sub(st.committedTime).Seconds()
			if age < 0 {
				age = 0
			}
			c.m.setLagSeconds(c.group, topic, ps, age)
		}
	}
}

// noteLagRefreshFailure counts and logs a failed refresh. The gauges keep their
// previous values — stale, not wrong.
func (c *Consumer) noteLagRefreshFailure(err error) {
	c.m.observeLagRefreshError(c.group)
	c.log.Warn("kafka consumer lag refresh failed; lag gauges are stale, not wrong",
		slog.String("code", errorCode(err)),
		slog.String("error", err.Error()),
	)
}

// ownedStates snapshots the per-partition state under the lock so the refresher
// never holds it across a network call.
func (c *Consumer) ownedStates() map[topicPartition]partitionState {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[topicPartition]partitionState, len(c.state))
	for topic, ps := range c.assigned {
		for p := range ps {
			tp := topicPartition{topic: topic, partition: p}
			if st := c.state[tp]; st != nil {
				out[tp] = *st
				continue
			}
			out[tp] = partitionState{}
		}
	}
	return out
}

// Close leaves the group and releases the client.
//
// # Leaving cleanly is not politeness, it is availability
//
// A member that disappears without a LeaveGroup request holds its partitions
// until the coordinator's session timeout expires — 45 seconds by default —
// and nothing consumes them in the meantime. kgo.Client.Close sends the leave
// request, so the group rebalances immediately and the next member picks the
// partitions up. That is the difference between a rolling update that is
// invisible and one that stalls the pipeline for the length of a session
// timeout per pod.
//
// CloseAllowingRebalance, not Close: with BlockRebalanceOnPoll a plain Close
// hangs if the last poll's rebalance was never allowed. Leaving the group
// causes a final revoke, and that revoke is where the last commit happens.
//
// Close is idempotent. It does not wait for Run to return; cancel Run's context
// first for an orderly shutdown.
func (c *Consumer) Close() error {
	if !c.closed.set() {
		return nil
	}
	// Releases BlockRebalanceOnPoll, sends LeaveGroup (which triggers a final
	// OnPartitionsRevoked, and therefore a final commit), then closes.
	c.cl.CloseAllowingRebalance()
	c.log.Info("kafka consumer closed")
	return nil
}

// -----------------------------------------------------------------------------
// Snapshot reads of compacted topics
// -----------------------------------------------------------------------------

// SnapshotOptions configures a Snapshotter.
type SnapshotOptions struct {
	ClientOptions

	// Topic is the compacted topic to read. Use TopicOddsNormalized or
	// TopicPriceComputed.
	Topic string

	// AllowUnregistered permits a snapshot read of a topic topics.go does not
	// declare.
	//
	// The registry knows the cleanup policy of the four declared topics and
	// nothing else, so for any other name this package cannot tell a compacted
	// log from a retention-based one — and a from-the-start read of a
	// retention-based topic is not a snapshot, it is whatever the retention
	// window happens to still hold. Setting this asserts the caller knows
	// better. It exists for two real cases: the integration tests, which create
	// their own throwaway compacted topics because a test that shares
	// Terraform's topics pollutes a live snapshot, and the signals.* topics a
	// phase-12 Flink job will own before the registry enumerates them.
	AllowUnregistered bool

	// Timeout bounds one whole Read. Zero means DefaultSnapshotTimeout.
	Timeout time.Duration
}

func (o SnapshotOptions) validate() error {
	if err := o.ClientOptions.validate(); err != nil {
		return err
	}
	if err := validateTopicName(o.Topic); err != nil {
		return err
	}
	if o.Timeout < 0 {
		return fmt.Errorf("%w: Timeout is negative", ErrInvalidOptions)
	}
	return nil
}

// resolveTopic applies the compaction guard.
func (o SnapshotOptions) resolveTopic() (Topic, error) {
	t, known := LookupTopic(o.Topic)
	if known {
		if !t.Compacted() {
			return Topic{}, fmt.Errorf("%w: %s is %s; reading it from the start yields whatever "+
				"the retention window still holds, which is not a snapshot",
				ErrNotCompacted, t.Name(), t.Retention())
		}
		return t, nil
	}
	if !o.AllowUnregistered {
		return Topic{}, fmt.Errorf("%w: %q is not a declared topic and its cleanup policy is "+
			"unknown; set AllowUnregistered to read it anyway", ErrNotCompacted, o.Topic)
	}
	return externalTopic(o.Topic), nil
}

// SnapshotStats describes one completed snapshot read.
type SnapshotStats struct {
	// Partitions is how many partitions the topic had when the read started.
	Partitions int
	// Values is the number of records carrying a payload.
	Values int
	// Tombstones is the number of deletions seen. On a topic that has been
	// running a while this being zero means nothing has ever been deleted, not
	// that deletions were missed.
	Tombstones int
	// Duration is wall-clock time for the whole read.
	Duration time.Duration
}

// Records is the total record count.
func (s SnapshotStats) Records() int { return s.Values + s.Tombstones }

// LogValue implements slog.LogValuer.
func (s SnapshotStats) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("partitions", s.Partitions),
		slog.Int("values", s.Values),
		slog.Int("tombstones", s.Tombstones),
		slog.String("duration", s.Duration.String()),
	)
}

// Snapshotter reads a compacted topic from the beginning to the end offsets
// observed at the start.
//
// CLAUDE.md §3 makes this load-bearing for the whole architecture: "a compacted
// topic keyed by market_id IS the current-line snapshot, replayable from
// scratch, which removes a whole class of cache-coherency bugs between the bus
// and Redis." This type is that sentence made executable.
//
// # It uses NO consumer group, deliberately
//
// A snapshot read commits no offsets, joins no group, and triggers no
// rebalance, so a service can rebuild its snapshot at startup — or an operator
// can rebuild it by hand — without disturbing the live consumers of the same
// topic. Using a group would have all three effects and would additionally make
// a second read of the same data impossible without resetting offsets.
//
// # Caught up has one meaning, and it is an offset, not a timeout
//
// "Done" is: for every partition, the next offset to read has reached the HIGH
// WATERMARK LISTED WHEN THE READ BEGAN. Records published while the read is in
// progress are therefore not chased — otherwise a busy topic would never finish
// — and the snapshot is a consistent-enough view as of the start instant, with
// the tail arriving through the ordinary consumer afterwards.
//
// An empty partition is complete immediately, and so is one whose entire log
// has been deleted by retention: the start offset is listed too, so a partition
// whose start already equals its end never waits for a record that is not
// coming. That case is why the end offsets alone are not enough.
type Snapshotter struct {
	healthChecker

	base    ClientOptions
	adm     *kadm.Client
	cl      *kgo.Client
	topic   Topic
	timeout time.Duration

	log    *slog.Logger
	m      *Metrics
	tracer trace.Tracer
	prop   propagation.TextMapPropagator

	closed closedFlag
}

// NewSnapshotter opens a snapshot reader for a compacted topic.
//
// The client it holds does not consume: it exists for the offset listing, for
// the readiness probe, and for nothing else. Each Read builds its own
// short-lived consuming client, which is what makes Read repeatable — a
// long-lived consumer would resume where the previous read stopped rather than
// starting over.
func NewSnapshotter(ctx context.Context, opts SnapshotOptions) (*Snapshotter, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	topic, err := opts.resolveTopic()
	if err != nil {
		return nil, err
	}
	m, err := opts.resolveMetrics()
	if err != nil {
		return nil, err
	}

	log := opts.Logger.With(
		slog.String("component", "kafka.snapshot"),
		slog.String("topic", topic.Name()),
	)

	cl, err := kgo.NewClient(opts.baseOpts()...)
	if err != nil {
		return nil, fmt.Errorf("%w: build snapshot client: %w", ErrInvalidOptions, err)
	}

	s := &Snapshotter{
		base:    opts.ClientOptions,
		adm:     kadm.NewClient(cl),
		cl:      cl,
		topic:   topic,
		timeout: positiveOr(opts.Timeout, DefaultSnapshotTimeout),
		log:     log,
		m:       m,
		tracer:  opts.tracer(),
		prop:    opts.propagator(),
	}
	s.healthChecker = healthChecker{
		cl:      cl,
		m:       m,
		log:     log,
		timeout: positiveOr(opts.ProbeTimeout, DefaultProbeTimeout),
		closed:  &s.closed,
	}

	if !opts.SkipStartupProbe {
		if err := awaitReady(ctx, cl, opts.ClientOptions, m, log); err != nil {
			cl.Close()
			return nil, err
		}
	}
	return s, nil
}

// Topic returns the topic this snapshotter reads.
func (s *Snapshotter) Topic() Topic { return s.topic }

// Read streams the whole compacted log to fn, in per-partition offset order,
// and returns once every partition has reached the end offset listed at the
// start.
//
// # Ordering is what makes this a fold
//
// A key always hashes to the same partition, and Kafka orders records within a
// partition, so applying records in the order fn receives them yields
// last-write-wins per key with no extra bookkeeping. Compaction is
// ASYNCHRONOUS, so the uncompacted tail legitimately holds several versions of
// one key — code that assumes one record per key on a compacted topic is code
// that works only after the log cleaner has run, which on a busy topic is never
// a safe assumption.
//
// fn must handle Delivery.Tombstone: it means DELETE this key from whatever is
// being built.
//
// An error from fn aborts the read and is returned wrapped. Delivery.Value()
// aliases the fetch buffer, so fn must copy anything it retains.
func (s *Snapshotter) Read(ctx context.Context, fn func(context.Context, *Delivery) error) (SnapshotStats, error) {
	var stats SnapshotStats
	if err := s.closed.err(); err != nil {
		return stats, err
	}
	if fn == nil {
		return stats, fmt.Errorf("%w: fn is nil", ErrInvalidOptions)
	}

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	ctx, span := s.tracer.Start(ctx, s.topic.Name()+" "+operationSnapshot,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(baseSpanAttrs(s.topic.Name(), operationSnapshot)...),
	)
	defer span.End()

	start := time.Now()
	stats, err := s.read(ctx, fn)
	stats.Duration = time.Since(start)

	s.m.observeSnapshot(s.topic.Name(), stats.Duration, stats.Values, stats.Tombstones)
	span.SetAttributes(attribute.Int(attrMessagingBatchSize, stats.Records()))
	recordSpanError(span, err)
	if err != nil {
		return stats, err
	}

	s.log.Info("kafka snapshot read complete", slog.Any("stats", stats))
	return stats, nil
}

// read is Read without the span, the timer or the metrics.
func (s *Snapshotter) read(ctx context.Context, fn func(context.Context, *Delivery) error) (SnapshotStats, error) {
	var stats SnapshotStats

	// The two listings together define "caught up". End alone is not enough: a
	// partition whose log has been fully deleted by retention has end > 0 and
	// no records to read, and would otherwise never complete.
	ends, err := s.adm.ListEndOffsets(ctx, s.topic.Name())
	if err != nil {
		return stats, fmt.Errorf("kafka: list end offsets for %s: %w", s.topic.Name(), err)
	}
	if err := ends.Error(); err != nil {
		return stats, fmt.Errorf("kafka: list end offsets for %s: %w", s.topic.Name(), err)
	}
	starts, err := s.adm.ListStartOffsets(ctx, s.topic.Name())
	if err != nil {
		return stats, fmt.Errorf("kafka: list start offsets for %s: %w", s.topic.Name(), err)
	}
	if err := starts.Error(); err != nil {
		return stats, fmt.Errorf("kafka: list start offsets for %s: %w", s.topic.Name(), err)
	}

	// next[p] is the offset still to be read; target[p] is where it must reach.
	next := make(map[int32]int64)
	target := make(map[int32]int64)
	for part, end := range ends[s.topic.Name()] {
		if part < 0 {
			// kadm reports a missing topic as a synthetic partition -1 carrying
			// UNKNOWN_TOPIC_OR_PARTITION, which ends.Error() has already
			// surfaced. Belt and braces.
			continue
		}
		from := int64(0)
		if st, ok := starts.Lookup(s.topic.Name(), part); ok && st.Offset > 0 {
			from = st.Offset
		}
		next[part] = from
		target[part] = end.Offset
	}
	if len(target) == 0 {
		return stats, fmt.Errorf("%w: %s has no partitions (has Terraform applied?)",
			ErrInvalidTopic, s.topic.Name())
	}
	stats.Partitions = len(target)

	if snapshotComplete(next, target) {
		return stats, nil
	}

	// A dedicated, non-group client per read: ConsumeTopics without
	// ConsumerGroup is a direct assignment of every partition, which commits
	// nothing and disturbs no live consumer.
	kopts := append(s.base.baseOpts(),
		kgo.ConsumeTopics(s.topic.Name()),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	cl, err := kgo.NewClient(kopts...)
	if err != nil {
		return stats, fmt.Errorf("%w: build snapshot consumer: %w", ErrInvalidOptions, err)
	}
	defer cl.Close()

	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			return stats, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return stats, fmt.Errorf("kafka: snapshot of %s did not reach the end offsets in %s "+
				"(%d/%d partitions complete): %w",
				s.topic.Name(), s.timeout, completeCount(next, target), len(target), err)
		}
		for _, fe := range fetches.Errors() {
			if errors.Is(fe.Err, context.Canceled) || errors.Is(fe.Err, context.DeadlineExceeded) {
				continue
			}
			// Not fatal: franz-go retries fetch errors internally and the
			// deadline above is what bounds the whole read.
			s.log.Warn("kafka snapshot fetch error",
				slog.Int("partition", int(fe.Partition)),
				slog.String("code", errorCode(fe.Err)),
				slog.String("error", fe.Err.Error()),
			)
		}

		var iterErr error
		fetches.EachPartition(func(ftp kgo.FetchTopicPartition) {
			if iterErr != nil {
				return
			}
			for _, rec := range ftp.Records {
				if rec.Offset >= target[rec.Partition] {
					// Published after the read began. Not chased — see the
					// type comment.
					continue
				}
				d, err := newDelivery(rec)
				if err != nil {
					// The `group` label is empty because a snapshot read has no
					// consumer group, by design. That is the honest value: an
					// invented group name would appear in the dashboard's
					// `sum by (group, topic)` as a member that does not exist.
					//
					// A decode failure is ALWAYS fatal here, whatever a
					// consumer's ErrorPolicy would say, because a snapshot with
					// a hole in it is not a snapshot — it is a current-line view
					// that is silently missing a market.
					s.m.observeDecodeError("", rec.Topic, err)
					iterErr = fmt.Errorf("kafka: snapshot of %s: %w", s.topic.Name(), err)
					return
				}
				if err := fn(ctx, d); err != nil {
					iterErr = fmt.Errorf("kafka: snapshot of %s at partition %d offset %d: %w",
						s.topic.Name(), rec.Partition, rec.Offset, err)
					return
				}
				if d.Tombstone {
					stats.Tombstones++
				} else {
					stats.Values++
				}
				next[rec.Partition] = rec.Offset + 1
			}
		})
		if iterErr != nil {
			return stats, iterErr
		}
		if snapshotComplete(next, target) {
			return stats, nil
		}
	}
}

// snapshotComplete reports whether every partition has reached its target.
func snapshotComplete(next, target map[int32]int64) bool {
	for part, want := range target {
		if next[part] < want {
			return false
		}
	}
	return true
}

// completeCount is snapshotComplete's per-partition tally, for the timeout
// message.
func completeCount(next, target map[int32]int64) int {
	n := 0
	for part, want := range target {
		if next[part] >= want {
			n++
		}
	}
	return n
}

// Snapshot reads the whole compacted log and folds it to the latest payload per
// key.
//
// It is the convenience form of Read for the common case: build the
// current-line snapshot in memory at startup. A tombstone DELETES its key from
// the result, which is what makes the returned map the state the log actually
// describes rather than a union of everything ever written.
//
// The payload bytes are COPIED — Delivery.Value() aliases the fetch buffer, so
// retaining it without a copy is a use-after-free in slow motion.
//
// Memory is bounded by the number of live keys, not by the message rate: that
// is the property compaction buys, and it is why this is affordable on
// odds.normalized, where one key is one market on the current slate. It would
// NOT be affordable on a retention-based topic, which is the other reason
// NewSnapshotter refuses one.
func (s *Snapshotter) Snapshot(ctx context.Context) (map[string]json.RawMessage, SnapshotStats, error) {
	out := make(map[string]json.RawMessage)
	stats, err := s.Read(ctx, func(_ context.Context, d *Delivery) error {
		if d.Tombstone {
			delete(out, d.Key)
			return nil
		}
		out[d.Key] = json.RawMessage(bytes.Clone(d.Envelope.Data))
		return nil
	})
	if err != nil {
		return nil, stats, err
	}
	return out, stats, nil
}

// Close releases the snapshotter's client. It is idempotent.
func (s *Snapshotter) Close() error {
	if !s.closed.set() {
		return nil
	}
	s.cl.Close()
	return nil
}
