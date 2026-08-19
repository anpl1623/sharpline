// The third consume shape: a GROUPLESS, from-the-beginning, continuous reader of
// a compacted topic.
//
// Consumer splits a topic's partitions between the members of a group.
// Snapshotter reads a compacted log once and stops. Follower is neither: it
// reads the WHOLE log, from the start, for ever, and it does so without joining
// anything.
//
// # Why a third shape exists at all
//
// `stream` is horizontally scalable and CLAUDE.md §9 forbids session affinity —
// "stream must be horizontally scalable, so no session affinity". A client's
// WebSocket therefore lands on an arbitrary replica, and that replica must be
// able to answer for ANY market on the slate.
//
// A consumer group cannot do that. A group divides the partitions among its
// members, so with two `stream` replicas each would hold half the slate. Nothing
// would fail loudly: a client that subscribed to a market owned by the other pod
// would simply receive an empty snapshot and no updates, for ever, and which
// half of the slate a client can see would depend on which pod its load balancer
// picked. That is the exact failure the affinity-free topology is supposed to
// make impossible.
//
// So every `stream` replica reads EVERY record. Direct partition assignment with
// no group and no committed offsets is what makes that possible: N followers of
// one topic do not divide anything, do not rebalance, and do not disturb the
// consumer groups that share the topic.
//
// # There is no seam between "snapshot" and "live"
//
// CLAUDE.md §3 chose a compacted topic precisely so that the replay IS the
// current state: "a compacted topic keyed by market_id IS the current-line
// snapshot, replayable from scratch, which removes a whole class of
// cache-coherency bugs between the bus and Redis."
//
// A Follower reads that log from offset zero and then keeps reading. The same
// poll loop delivers the historical fold and the live tail, in one uninterrupted
// offset order per partition, through one handler. The classic bug this removes
// is the lost update between a snapshot read and a subscription: with a separate
// Snapshotter and Consumer there is a window between "the snapshot ended at
// offset N" and "the consumer started at offset M", and every record in it is
// either dropped (M > N) or applied twice (M < N). Here there is no window,
// because there is no second reader.
//
// That is also why `stream` holds NO Snapshotter. A client's snapshot is taken
// from the hub's in-memory fold under the same mutex that serialises delta
// publication, not from a second read of the bus.
//
// # Catch-up is an offset comparison, not a timer
//
// "Caught up" means: for every partition, the next offset to read has reached
// the END OFFSET LISTED WHEN Run BEGAN. Records published while the replay is in
// progress are not chased — they arrive through the same loop afterwards, which
// is the whole point — but they ARE delivered as they arrive, and delivering one
// also advances its partition past the target, which is how a busy partition
// finishes at all.
//
// Both the start and the end offsets are listed, for the reason Snapshotter
// gives: a partition whose log was entirely removed by retention has end > 0 and
// nothing to read, and comparing against the end alone would make it wait for a
// record that is never coming. An empty topic is caught up before the first
// poll.
//
// Missing the catch-up deadline is an ERROR from Run, not a warning. A `stream`
// replica that reported ready holding a partial slate would serve incomplete
// snapshots to real clients — the same silent half-slate failure the group was
// rejected for, arrived at by a different route.
//
// # There is no consumer lag to export, and that is deliberate
//
// sharpline_kafka_consumer_lag_records and _lag_seconds are aggregated
// `by (group, topic)` on the dashboard and behind two alert rules. A follower
// belongs to no group, so it has no offsets committed anywhere and no group lag
// to report. Emitting either series with an invented group name would put a
// member that does not exist into those aggregates; emitting them with an empty
// group would put a phantom row on the panel. Neither is done. DO NOT ADD ONE.
//
// A follower-specific lag gauge was considered and is not exported either:
//
//   - The cheap source is the high watermark franz-go attaches to every fetch,
//     and Consumer.refreshLag already writes down why that source is not
//     trustworthy: franz-go surfaces a fetch only for a partition that HAS
//     records or errors, so a partition that is idle or caught up never appears,
//     and its gauge would sit at its last value for ever. A stale gauge is worse
//     than a missing one, because it is indistinguishable from a measurement.
//   - The honest source is a periodic ListEndOffsets poll compared against this
//     reader's own position — which costs a second background goroutine and a
//     new registered collector, for a question that is already answered better
//     elsewhere. sharpline_odds_staleness_seconds{stage="fanout"} is the headline
//     SLO (CLAUDE.md §9) and it measures the same lateness end to end, in
//     seconds of price age rather than in records. A record count would be a
//     second, weaker answer to a question the SLO already answers.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// DefaultFollowerMaxPollRecords bounds one delivery batch.
//
// It is NOT DefaultMaxPollRecords under another name, even though the number is
// the same. That constant is a rebalance-safety parameter — the group cannot
// rebalance until the batch has been handled and committed, so exceeding the
// rebalance timeout fences the member. None of that exists here: there is no
// group, nothing is blocked on this loop, and no other process is waiting.
//
// What it bounds here is how long the loop goes between completion checks, and
// how many records are held in the fetch buffer at once. The completion check
// runs once per batch, so an unbounded batch would only delay the CaughtUp
// signal past the instant it was actually true.
const DefaultFollowerMaxPollRecords = 500

// Backoff for the wait on a topic that has not been created yet. See
// awaitCatchUpTargets.
//
// It starts short because the common case in compose is a gap of a few hundred
// milliseconds between `compose up` and `make topics`, and it doubles to a
// ceiling because the uncommon case is a cluster where a human has not applied
// Terraform at all and the right cadence there is "quietly, once a second"
// rather than a hot loop against the broker's metadata endpoint.
const (
	topicWaitBackoff    = 250 * time.Millisecond
	topicWaitBackoffMax = time.Second

	// topicWaitLogEvery throttles the repeat warning. The first attempt always
	// logs; after that one line roughly every 20 seconds at the ceiling is
	// enough to show the wait is still happening without burying the log.
	topicWaitLogEvery = 20
)

// followerGroup is the `group` metric label every follower observation carries:
// the empty string, because a follower joins no group.
//
// Snapshotter set this precedent and stated the reason — an invented group name
// "would appear in the dashboard's `sum by (group, topic)` as a member that does
// not exist". The empty string is the honest value: it reads as "no group" and
// cannot be mistaken for a real one. None of the series a follower touches
// (consume_records_total, consume_bytes_total, handler_duration_seconds,
// decode_errors_total, fetch_errors_total, snapshot_*) is on the dashboard or in
// the alert rules, so no aggregate is disturbed by the extra label value.
const followerGroup = ""

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// FollowerOptions configures a Follower.
type FollowerOptions struct {
	ClientOptions

	// Topic is the compacted topic to follow. Required. `stream` follows
	// TopicPriceComputed.
	Topic string

	// AllowUnregistered permits following a topic topics.go does not declare.
	//
	// It carries exactly the meaning SnapshotOptions.AllowUnregistered carries,
	// because the guard is literally the same one: the registry knows the
	// cleanup policy of the declared topics and nothing else, so for any other
	// name this package cannot tell a compacted log from a retention-based one,
	// and following a retention-based topic from the start yields whatever the
	// retention window happens to still hold rather than a slate. Setting this
	// asserts the caller knows better. It exists for the integration tests,
	// which create throwaway compacted topics, and for the signals.* topics a
	// phase-12 Flink job will own.
	AllowUnregistered bool

	// ErrorPolicy decides what a failing record does AFTER catch-up. Zero value
	// is ErrorPolicyStop.
	//
	// DURING catch-up the policy is ignored and every failure is fatal —
	// Snapshotter's reason, unchanged: "a snapshot with a hole in it is not a
	// snapshot — it is a current-line view that is silently missing a market."
	// That applies to a handler that failed to fold a record into the slate
	// exactly as it applies to a record that would not decode, so both are
	// fatal while the replay is in progress. ErrorPolicySkip therefore takes
	// effect only once the slate is known to be complete.
	ErrorPolicy ErrorPolicy

	// CatchUpTimeout bounds the CATCH-UP PHASE ONLY. Zero means
	// DefaultSnapshotTimeout.
	//
	// It is not a deadline on Run, which blocks for the life of the process. It
	// is the number a Kubernetes startupProbe has to be sized against, because
	// it is how long a replica may take to hold the full slate before it is
	// declared broken rather than slow.
	CatchUpTimeout time.Duration

	// MaxPollRecords bounds one delivery batch. Zero means
	// DefaultFollowerMaxPollRecords.
	MaxPollRecords int
}

func (o FollowerOptions) validate() error {
	if err := o.ClientOptions.validate(); err != nil {
		return err
	}
	if err := validateTopicName(o.Topic); err != nil {
		return err
	}
	if !o.ErrorPolicy.Valid() {
		return fmt.Errorf("%w: ErrorPolicy %d is not a policy this package implements",
			ErrInvalidOptions, o.ErrorPolicy)
	}
	if o.CatchUpTimeout < 0 {
		return fmt.Errorf("%w: CatchUpTimeout is negative", ErrInvalidOptions)
	}
	return nil
}

// resolveTopic applies the compaction guard.
//
// It delegates to SnapshotOptions.resolveTopic rather than restating the rule,
// so the two cannot drift: a follower reads the log from the start for the same
// reason a snapshot does, so "a from-the-start read of a retention-based topic
// is not a snapshot" is one rule with one implementation, not two copies that
// will eventually disagree about whether odds.raw.synthetic is followable.
func (o FollowerOptions) resolveTopic() (Topic, error) {
	return SnapshotOptions{Topic: o.Topic, AllowUnregistered: o.AllowUnregistered}.resolveTopic()
}

// -----------------------------------------------------------------------------
// Follower
// -----------------------------------------------------------------------------

// Follower reads a compacted topic from the beginning and keeps reading.
//
// See the file header for the argument. In one sentence: it is the read shape
// that lets N replicas of a stateless service each hold the entire slate, which
// a consumer group cannot do because a group divides the partitions.
//
// # The Handler contract, restated because it is easy to get wrong here
//
//   - IT MUST HANDLE A TOMBSTONE. Delivery.Tombstone means the key is gone for
//     ever. On this topic that is a market leaving the slate, and a handler that
//     ignores it leaves a dead market in the hub's fold permanently — no further
//     record for that key is coming.
//   - IT MUST NOT RETAIN Delivery.Value() OR Envelope.Data past its return.
//     Those bytes alias the fetch buffer. The hub keeps market documents for the
//     life of the market, so it must copy them (bytes.Clone), exactly as
//     Snapshotter.Snapshot does.
//   - IT MUST RETURN PROMPTLY. The loop is sequential and single-goroutine, so
//     handler time is fanout latency and shows up in the staleness SLO.
//   - IDEMPOTENCE is not required for correctness here the way it is for
//     Consumer — nothing is redelivered, because nothing is committed and
//     nothing rebalances — but the fold is last-write-wins by key anyway, so it
//     comes free.
//
// A Follower is used exactly once: build it, Run it, Close it. Run cannot be
// called twice, because the client holds its consume position and a second Run
// would resume rather than replay.
type Follower struct {
	healthChecker

	adm   *kadm.Client
	cl    *kgo.Client
	topic Topic

	errorPolicy    ErrorPolicy
	catchUpTimeout time.Duration
	maxPollRecords int

	log    *slog.Logger
	m      *Metrics
	tracer trace.Tracer
	prop   propagation.TextMapPropagator

	// caughtUp is closed exactly once, by Run, at the instant every partition
	// has reached the end offset listed when Run began.
	caughtUp     chan struct{}
	caughtUpOnce sync.Once

	// mu guards the published catch-up stats. Run mutates its own local copy
	// while the replay is in progress and publishes it here once; a reader
	// therefore never observes a half-counted replay.
	mu       sync.Mutex
	stats    SnapshotStats
	statsSet bool

	started atomic.Bool
	closed  closedFlag
}

// checker is asserted for *Follower here rather than in health.go's var block so
// that this file is self-contained; the assertion exists for the reason stated
// there — a signature change in httpx.Checker must break the build rather than
// silently drop this client out of a service's readiness list.
var _ checker = (*Follower)(nil)

// NewFollower opens a groupless reader of a compacted topic and proves it can
// reach the cluster.
//
// It consumes nothing until Run is called. The client is built with
// ConsumeTopics and NO ConsumerGroup, which in franz-go is a DIRECT ASSIGNMENT
// of every partition of the topic: it joins nothing, commits nothing, triggers
// no rebalance, and is invisible to every other consumer of the same topic. It
// also picks up partitions added later, on the next metadata refresh, which
// ConsumePartitions with a fixed partition list would not — and Terraform
// raising a topic's partition count must not silently leave a slice of the slate
// unread.
//
// kgo.DisableAutoCommit is deliberately NOT set. Auto-commit is a group concept;
// franz-go rejects the option outright without a group ("invalid autocommit
// options specified when a group was not specified"), which is the strongest
// possible statement that there is nothing here to commit.
func NewFollower(ctx context.Context, opts FollowerOptions) (*Follower, error) {
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
		slog.String("component", "kafka.follower"),
		slog.String("topic", topic.Name()),
	)

	kopts := append(opts.baseOpts(),
		kgo.ConsumeTopics(topic.Name()),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
	)
	cl, err := kgo.NewClient(kopts...)
	if err != nil {
		return nil, fmt.Errorf("%w: build follower client: %w", ErrInvalidOptions, err)
	}

	f := &Follower{
		adm:            kadm.NewClient(cl),
		cl:             cl,
		topic:          topic,
		errorPolicy:    opts.ErrorPolicy,
		catchUpTimeout: positiveOr(opts.CatchUpTimeout, DefaultSnapshotTimeout),
		maxPollRecords: positiveIntOr(opts.MaxPollRecords, DefaultFollowerMaxPollRecords),
		log:            log,
		m:              m,
		tracer:         opts.tracer(),
		prop:           opts.propagator(),
		caughtUp:       make(chan struct{}),
	}
	f.healthChecker = healthChecker{
		cl:      cl,
		m:       m,
		log:     log,
		timeout: positiveOr(opts.ProbeTimeout, DefaultProbeTimeout),
		closed:  &f.closed,
	}

	if !opts.SkipStartupProbe {
		if err := awaitReady(ctx, cl, opts.ClientOptions, m, log); err != nil {
			cl.Close()
			return nil, err
		}
	}
	return f, nil
}

// Topic returns the topic this follower reads.
func (f *Follower) Topic() Topic { return f.topic }

// CaughtUp returns a channel that is closed once the compacted log has been
// replayed in full — i.e. once every partition has reached the end offset listed
// when Run began.
//
// It is the readiness gate for a service whose correctness depends on holding
// the whole slate. A `stream` replica must not report ready before this closes,
// because a client that connected earlier would get a snapshot that is missing
// markets and would never be told.
//
// IT IS NOT CLOSED IF Run FAILS. A caller must therefore select on it alongside
// whatever channel carries Run's error (and alongside the process context),
// rather than blocking on it alone — a Run that could not reach the end offsets
// inside CatchUpTimeout returns an error and this channel stays open for ever,
// which is the correct report of "this replica never had the slate".
func (f *Follower) CaughtUp() <-chan struct{} { return f.caughtUp }

// HasCaughtUp reports whether the initial replay has completed, without
// blocking.
func (f *Follower) HasCaughtUp() bool {
	select {
	case <-f.caughtUp:
		return true
	default:
		return false
	}
}

// CatchUpStats reports what the initial replay saw, and whether it completed.
//
// The stats are meaningless before completion and are reported as (zero, false)
// until then — deliberately, rather than exposing a partial count that would
// look like a finished one. Values and Tombstones count every record DELIVERED
// during the replay, which on a busy topic includes a few published after the
// targets were listed; they were handled, so counting them is the honest report
// of what the handler saw.
func (f *Follower) CatchUpStats() (SnapshotStats, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats, f.statsSet
}

// Run replays the compacted log and then follows it live, delivering every
// record to h in per-partition offset order, until ctx is cancelled or Close is
// called.
//
// It returns nil for a clean stop and the failure otherwise. The failures are:
// the catch-up deadline expiring before the slate is complete, a record that
// would not decode, and a handler error the ErrorPolicy does not tolerate. It
// blocks, so a cmd/ entrypoint runs it in a goroutine alongside the httpx server
// and cancels the shared context on SIGTERM.
//
// Run may be called ONCE per Follower.
func (f *Follower) Run(ctx context.Context, h Handler) error {
	if h == nil {
		return fmt.Errorf("%w: Handler is nil", ErrInvalidOptions)
	}
	if err := f.closed.err(); err != nil {
		return err
	}
	if !f.started.CompareAndSwap(false, true) {
		// Not a mutex: a second Run is not something to serialise, it is
		// something to refuse. The client holds its consume position, so a
		// second Run would continue from wherever the first stopped and would
		// never replay the log — it would report itself caught up while holding
		// only the tail.
		return fmt.Errorf("kafka: follower of %s has already run; build a new Follower to replay the log",
			f.topic.Name())
	}

	f.log.Info("kafka follower starting",
		slog.String("error_policy", f.errorPolicy.String()),
		slog.String("catch_up_timeout", f.catchUpTimeout.String()),
		slog.Int("max_poll_records", f.maxPollRecords),
	)

	err := f.run(ctx, h)
	if err != nil {
		f.log.Error("kafka follower stopping on error", slog.String("error", err.Error()))
		return err
	}
	f.log.Info("kafka follower stopped", slog.Bool("caught_up", f.HasCaughtUp()))
	return nil
}

// run is Run without the guards and the lifecycle logging.
func (f *Follower) run(ctx context.Context, h Handler) (err error) {
	// The catch-up deadline bounds the replay and NOTHING ELSE. Once the slate
	// is complete the loop switches back to the caller's context, which is what
	// makes CatchUpTimeout a startup budget rather than a lifetime.
	catchCtx, cancelCatchUp := context.WithTimeout(ctx, f.catchUpTimeout)
	defer cancelCatchUp()

	// One span for the whole replay: "how long did this replica take to hold the
	// full slate" is a real startup question and this is where it is answerable.
	//
	// The context the tracer returns is DISCARDED on purpose. Per-record spans
	// join the PRODUCER's trace through the record headers (see otel.go), and a
	// record produced without tracing headers would otherwise fall back to this
	// context and become a child of the catch-up span — giving it one child per
	// record on the compacted log, which otel.go already names as the shape
	// nobody opens in Jaeger.
	_, span := f.tracer.Start(ctx, f.topic.Name()+" "+operationSnapshot,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(baseSpanAttrs(f.topic.Name(), operationSnapshot)...),
	)
	spanOpen := true
	endSpan := func(cause error) {
		if !spanOpen {
			return
		}
		spanOpen = false
		recordSpanError(span, cause)
		span.End()
	}
	// Covers every path that leaves before catch-up completed: a cancelled
	// context, a closed client, a decode failure, an expired deadline.
	defer func() { endSpan(err) }()

	start := time.Now()
	next, target, err := f.awaitCatchUpTargets(ctx, catchCtx)
	if err != nil {
		// A caller that cancelled before the targets could be listed is a
		// shutdown, not a failure — the same rule Consumer.Run applies to a
		// cancellation surfaced through a fetch. Checked against the CALLER's
		// context, not against the error, so a catch-up deadline that genuinely
		// expired here is still reported as one.
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	stats := SnapshotStats{Partitions: len(target)}

	// An empty topic — every partition's start already at its end — is caught up
	// before a single fetch. The alternative is a service that waits out its
	// whole startup budget on a slate that genuinely has nothing in it.
	caught := snapshotComplete(next, target)
	if caught {
		f.completeCatchUp(stats, time.Since(start))
		endSpan(nil)
		cancelCatchUp()
	}

	for {
		if ctx.Err() != nil || f.closed.isSet() {
			return nil
		}

		pollCtx := ctx
		if !caught {
			pollCtx = catchCtx
		}
		fetches := f.cl.PollRecords(pollCtx, f.maxPollRecords)

		if fetches.IsClientClosed() {
			return nil
		}
		// The caller's cancellation is a clean stop and is checked FIRST, so a
		// SIGTERM arriving mid-replay is reported as a shutdown rather than as a
		// failed catch-up.
		if ctx.Err() != nil {
			return nil
		}
		if !caught && catchCtx.Err() != nil {
			return fmt.Errorf("kafka: follower of %s did not reach the end offsets in %s "+
				"(%d/%d partitions complete); a replica holding a partial slate would serve "+
				"incomplete snapshots: %w",
				f.topic.Name(), f.catchUpTimeout,
				completeCount(next, target), len(target), catchCtx.Err())
		}

		f.observeFetchErrors(fetches)

		var loopErr error
		fetches.EachPartition(func(ftp kgo.FetchTopicPartition) {
			if loopErr != nil {
				return
			}
			for _, rec := range ftp.Records {
				d, rerr := f.handleRecord(ctx, h, rec, caught)
				if rerr != nil {
					loopErr = rerr
					return
				}
				if caught {
					continue
				}
				// A record whose offset is at or past the target is DELIVERED,
				// not skipped — that is the one place this differs from
				// Snapshotter, and it is the difference that removes the
				// lost-update window. It is a live delta that happens to have
				// arrived during the replay, and skipping it would drop an
				// update no later record is obliged to restate. Delivering it
				// also advances the partition past its target, which is how a
				// busy partition ever finishes catching up.
				next[rec.Partition] = rec.Offset + 1
				if d == nil {
					continue
				}
				if d.Tombstone {
					stats.Tombstones++
				} else {
					stats.Values++
				}
			}
		})
		if loopErr != nil {
			return loopErr
		}

		if !caught && snapshotComplete(next, target) {
			caught = true
			f.completeCatchUp(stats, time.Since(start))
			endSpan(nil)
			// Releases the deadline's timer immediately rather than at return,
			// which for this type is process lifetime.
			cancelCatchUp()
		}
	}
}

// awaitCatchUpTargets lists the catch-up targets, waiting out a topic that does
// not exist YET.
//
// # Why waiting is correct and failing is not
//
// CLAUDE.md §9 creates topics with Terraform, and Kafka runs with
// auto-topic-creation OFF. Nothing sequences that against a service start: in
// compose `make up` converges the topics between two `compose up` calls, and in
// Kubernetes the topic job and this Deployment are independent objects that a
// single `helm upgrade` starts together. So "the declared topic is not there
// yet" is an ORDINARY STARTUP STATE, not a failure — exactly as a Redis still
// replaying its AOF is in internal/platform/redis, which retries for the same
// reason.
//
// Returning the error instead is what this used to do, and it broke a property
// the rest of the stack has. The Makefile states that property out loud: "The
// services DO recover from that on their own (the scheduler backs off and
// retries, and the normalizer's warm start is lazy), so this ordering is not
// load-bearing for correctness." A Follower that gave up made `stream` the one
// service for which the ordering WAS load-bearing — and not merely at startup:
// the hub stopped, the process stayed alive, /readyz stayed red for ever, and
// the topic appearing thirty seconds later fixed nothing. Only a restart did.
// It was found by running the stack, not by a test, which is why the live gate
// exists.
//
// # Why an unbounded wait is safe here specifically
//
// A misspelt topic cannot reach this function. NewFollower resolves the name
// through LookupTopic and refuses anything the registry does not declare (or
// anything not compacted), so the only way to arrive here with a missing topic
// is a DECLARED topic that has not been created yet. The wait is therefore
// bounded by the thing it is waiting for, and it is bounded by the caller's
// context in every other sense — a SIGTERM ends it immediately.
//
// It is also not silent. Readiness stays red for the whole wait, because
// Hub.Check reports "not running" until catch-up completes, so an orchestrator
// keeps the replica out of rotation and a human sees the reason on the first
// log line rather than the four-hundredth.
//
// # The catch-up budget does not start until the topic exists
//
// waitCtx is the CALLER's context and catchCtx is the replay deadline. Listing
// under the replay deadline would spend the startup budget waiting for
// Terraform and then report "did not reach the end offsets in 60s" about a
// topic it never got to read — a true statement about the wrong thing.
func (f *Follower) awaitCatchUpTargets(waitCtx, catchCtx context.Context) (next, target map[int32]int64, err error) {
	var (
		attempt int
		logged  bool
		backoff = topicWaitBackoff
	)
	for {
		next, target, err = f.listCatchUpTargets(catchCtx)
		if err == nil {
			if logged {
				f.log.Info("topic exists; beginning the catch-up replay",
					slog.Int("waited_attempts", attempt),
					slog.Int("partitions", len(target)))
			}
			return next, target, nil
		}
		if !isTopicNotCreatedYet(err) {
			return nil, nil, err
		}
		attempt++
		if !logged {
			logged = true
			f.log.Warn("the topic does not exist yet; waiting for it rather than failing",
				slog.String("error", err.Error()),
				slog.String("expected_creator", "Terraform (compose: `make topics`; cluster: the topic Job)"),
				slog.String("meanwhile", "readiness stays red, so this replica takes no traffic"),
			)
		} else if attempt%topicWaitLogEvery == 0 {
			f.log.Warn("still waiting for the topic to be created",
				slog.Int("attempt", attempt),
				slog.String("topic", f.topic.Name()))
		}

		if err := sleepCtx(waitCtx, backoff); err != nil {
			// The caller gave up. A shutdown, not a failure.
			return nil, nil, err
		}
		if backoff < topicWaitBackoffMax {
			backoff *= 2
			if backoff > topicWaitBackoffMax {
				backoff = topicWaitBackoffMax
			}
		}
	}
}

// isTopicNotCreatedYet reports whether err means "this declared topic has not
// been created yet" rather than a real fault.
//
// Two spellings reach here and both mean the same thing. The broker answers
// UNKNOWN_TOPIC_OR_PARTITION, which kadm surfaces on a synthetic partition -1;
// and listCatchUpTargets' own ErrInvalidTopic fires when the listing succeeded
// but described no partitions, which is the shape a metadata response takes for
// a topic the cluster has just been told about and not yet materialised.
func isTopicNotCreatedYet(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInvalidTopic) {
		return true
	}
	var kerrErr *kerr.Error
	if errors.As(err, &kerrErr) {
		return kerrErr.Code == kerr.UnknownTopicOrPartition.Code
	}
	return false
}

// sleepCtx waits for d or until ctx is done, whichever comes first. It is the
// same helper internal/platform/redis carries, for the same startup-retry
// reason; it is duplicated rather than shared because a four-line timer is not
// worth an import edge between two platform packages.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// listCatchUpTargets lists where each partition starts and where it must reach.
//
// BOTH listings are needed, and the start listing is the non-obvious one: a
// partition whose entire log was deleted by retention has end > 0 and no records
// to read, so comparing the read position against the end alone would wait for a
// record that is not coming and burn the whole catch-up budget on it. That case
// is rare on a compacted topic and it is exactly the case a compacted topic with
// a delete.retention.ms hits after a tombstone sweep.
func (f *Follower) listCatchUpTargets(ctx context.Context) (next, target map[int32]int64, err error) {
	name := f.topic.Name()

	ends, err := f.adm.ListEndOffsets(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("kafka: list end offsets for %s: %w", name, err)
	}
	if err := ends.Error(); err != nil {
		return nil, nil, fmt.Errorf("kafka: list end offsets for %s: %w", name, err)
	}
	starts, err := f.adm.ListStartOffsets(ctx, name)
	if err != nil {
		return nil, nil, fmt.Errorf("kafka: list start offsets for %s: %w", name, err)
	}
	if err := starts.Error(); err != nil {
		return nil, nil, fmt.Errorf("kafka: list start offsets for %s: %w", name, err)
	}

	next = make(map[int32]int64)
	target = make(map[int32]int64)
	for part, end := range ends[name] {
		if part < 0 {
			// kadm reports a missing topic as a synthetic partition -1 carrying
			// UNKNOWN_TOPIC_OR_PARTITION, which ends.Error() has already
			// surfaced. Belt and braces.
			continue
		}
		from := int64(0)
		if st, ok := starts.Lookup(name, part); ok && st.Offset > 0 {
			from = st.Offset
		}
		next[part] = from
		target[part] = end.Offset
	}
	if len(target) == 0 {
		return nil, nil, fmt.Errorf("%w: %s has no partitions (has Terraform applied?)",
			ErrInvalidTopic, name)
	}
	return next, target, nil
}

// completeCatchUp publishes the replay's stats, signals CaughtUp and records the
// snapshot metrics. It runs at most once per Follower.
//
// The stats are published BEFORE the channel closes, so a goroutine that wakes
// on CaughtUp and immediately calls CatchUpStats cannot observe the zero value —
// the close is the happens-before edge that makes the publication visible.
//
// The metrics are sharpline_kafka_snapshot_duration_seconds and
// sharpline_kafka_snapshot_records_total, reused rather than duplicated under a
// follower-specific name: the quantity is identical — the time to read a
// compacted topic from the beginning to the high watermark observed at the start
// — and one series measuring one thing is worth more than two measuring it under
// different names.
func (f *Follower) completeCatchUp(stats SnapshotStats, elapsed time.Duration) {
	stats.Duration = elapsed

	f.mu.Lock()
	f.stats = stats
	f.statsSet = true
	f.mu.Unlock()

	f.m.observeSnapshot(f.topic.Name(), stats.Duration, stats.Values, stats.Tombstones)
	f.caughtUpOnce.Do(func() { close(f.caughtUp) })

	f.log.Info("kafka follower caught up; the compacted log has been replayed in full",
		slog.Any("stats", stats))
}

// handleRecord decodes one record and hands it to the handler.
//
// It returns the Delivery so the caller can tally the replay, and (nil, nil) for
// a record that was skipped under ErrorPolicySkip — which cannot happen during
// catch-up, because nothing is skippable while the slate is still being built.
func (f *Follower) handleRecord(ctx context.Context, h Handler, rec *kgo.Record, caughtUp bool) (*Delivery, error) {
	d, err := newDelivery(rec)
	if err != nil {
		f.m.observeDecodeError(followerGroup, rec.Topic, err)
		f.log.Error("kafka record could not be decoded",
			slog.String("topic", rec.Topic),
			slog.Int("partition", int(rec.Partition)),
			slog.Int64("offset", rec.Offset),
			slog.String("key", string(rec.Key)),
			slog.String("reason", decodeFailureReason(err)),
			slog.Bool("during_catch_up", !caughtUp),
			slog.String("error", err.Error()),
		)
		if !caughtUp {
			return nil, fmt.Errorf("kafka: catch-up of %s: %w", f.topic.Name(), err)
		}
		if f.errorPolicy == ErrorPolicyStop {
			return nil, fmt.Errorf("kafka: follower of %s: %w", f.topic.Name(), err)
		}
		// Skipping is the only way past an undecodable record: retrying cannot
		// change the bytes on disk, and there is no offset to leave uncommitted
		// here, so the choice is skip or stop.
		return nil, nil
	}

	f.m.observeConsumed(followerGroup, rec.Topic, recordBytes(rec))

	// The handler's span is a CHILD of the producer's, joined through the record
	// headers, which is what makes CLAUDE.md §9's "ingest → pricer → stream" one
	// trace rather than three.
	hctx := extractTrace(ctx, f.prop, rec)
	hctx, span := f.tracer.Start(hctx, rec.Topic+" "+operationProcess,
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(f.spanAttrs(d)...),
	)

	start := time.Now()
	herr := h.HandleMessage(hctx, d)
	elapsed := time.Since(start)

	recordSpanError(span, herr)
	span.End()
	f.m.observeHandler(followerGroup, rec.Topic, elapsed, herr)

	if herr != nil {
		f.log.Error("kafka message handler failed",
			slog.Any("record", d),
			slog.String("duration", elapsed.String()),
			slog.Bool("during_catch_up", !caughtUp),
			slog.String("error", herr.Error()),
		)
		// During catch-up the policy does not get a vote: a handler that failed
		// to fold a record into the slate leaves the same hole a decode failure
		// would, and a slate with a hole in it is not a slate.
		if !caughtUp {
			return nil, fmt.Errorf("kafka: catch-up of %s: %w: %s/%d offset %d: %w",
				f.topic.Name(), ErrHandlerFailed, rec.Topic, rec.Partition, rec.Offset, herr)
		}
		if f.errorPolicy == ErrorPolicyStop {
			return nil, fmt.Errorf("%w: %s/%d offset %d: %w",
				ErrHandlerFailed, rec.Topic, rec.Partition, rec.Offset, herr)
		}
	}
	return d, nil
}

// spanAttrs builds the per-record span attributes.
//
// There is no messaging.consumer.group.name attribute, and its absence is the
// point: a follower has no group, and an empty-string attribute would read in
// Jaeger as a group whose name happens to be blank rather than as no group at
// all.
func (f *Follower) spanAttrs(d *Delivery) []attribute.KeyValue {
	attrs := baseSpanAttrs(d.Topic, operationProcess)
	attrs = append(attrs,
		attribute.String(attrMessagingKey, d.Key),
		attribute.Int(attrMessagingPartition, int(d.Partition)),
		attribute.Int64(attrMessagingOffset, d.Offset),
		attribute.Int(attrMessagingBodySize, len(d.Value())),
	)
	if d.Tombstone {
		return append(attrs,
			attribute.Bool(attrTombstone, true),
			attribute.String(attrTombstoneReason, d.TombstoneReason),
		)
	}
	return append(attrs,
		attribute.String(attrMessageType, d.Envelope.Type),
		attribute.Int(attrEnvelopeVersion, d.Envelope.Version),
	)
}

// observeFetchErrors records per-partition fetch failures.
//
// None of them stop the loop: franz-go retries fetch errors internally, so the
// honest signal is a RATE rather than an occurrence. During catch-up the
// deadline is what turns a partition that never succeeds into a failure, and
// after catch-up a persistent rate here is the symptom to alert on.
func (f *Follower) observeFetchErrors(fetches kgo.Fetches) {
	for _, fe := range fetches.Errors() {
		if errors.Is(fe.Err, context.Canceled) ||
			errors.Is(fe.Err, context.DeadlineExceeded) ||
			errors.Is(fe.Err, kgo.ErrClientClosed) {
			continue
		}
		f.m.observeFetchError(followerGroup, fe.Topic, fe.Err)
		f.log.Warn("kafka follower fetch error",
			slog.String("topic", fe.Topic),
			slog.Int("partition", int(fe.Partition)),
			slog.String("code", errorCode(fe.Err)),
			slog.String("error", fe.Err.Error()),
		)
	}
}

// Close releases the follower's client. It is idempotent.
//
// Plain Close, not CloseAllowingRebalance: there is no group to leave, no
// generation to fence and no blocked rebalance to release. Consumer.Close's
// availability argument — a member that vanishes without a LeaveGroup holds its
// partitions until the session timeout expires — has no analogue here, because
// nothing ever joined and no other reader is waiting on anything this one holds.
//
// Close does not wait for Run to return; cancel Run's context first for an
// orderly shutdown.
func (f *Follower) Close() error {
	if !f.closed.set() {
		return nil
	}
	f.cl.Close()
	f.log.Info("kafka follower closed")
	return nil
}
