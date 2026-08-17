package kafka

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
)

// The BROKER-FREE half of the consume side.
//
// The behaviour CLAUDE.md §10 calls out — "the interesting bugs live in
// consumer-group rebalancing and offset handling" — is proven against a real
// broker in test/integration/kafka_test.go and cannot be proven anywhere else.
// What is here is the part that is decided without a network: option validation,
// the per-partition bookkeeping the commit boundary is built on, the snapshot
// termination arithmetic, and the ONE behavioural difference between
// OnPartitionsRevoked and OnPartitionsLost — which is observable without a
// broker precisely because the difference is whether a commit is attempted at
// all.

// validConsumerOptions is the smallest ConsumerOptions that passes validation.
func validConsumerOptions() ConsumerOptions {
	return ConsumerOptions{
		ClientOptions: validOptions(),
		Group:         "sharpline-test",
		Topics:        []string{TopicOddsNormalized},
	}
}

// counterTotal reads one counter sample by name and label values, and reports
// zero when the series has never been touched.
//
// "Never touched" and "touched and zero" are the same thing for this file's
// purposes and are deliberately not distinguished: what the callback tests
// assert is a COUNT, and a series that was never created is a count of nothing.
func counterTotal(t *testing.T, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			if !labelsMatch(m, labels) {
				continue
			}
			total += m.GetCounter().GetValue()
		}
	}
	return total
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, l := range m.GetLabel() {
		got[l.GetName()] = l.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// TestConsumerOptionsValidate covers the startup gate.
//
// The topic-name check is the one worth having: with auto-creation disabled
// (CLAUDE.md §9) a typo would otherwise surface as an
// UNKNOWN_TOPIC_OR_PARTITION several seconds into a poll loop, attached to no
// particular line of code.
func TestConsumerOptionsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*ConsumerOptions)
		wantErr error
		wantMsg string
	}{
		{name: "valid", mutate: func(*ConsumerOptions) {}},
		{
			name:    "empty group",
			mutate:  func(o *ConsumerOptions) { o.Group = "" },
			wantErr: ErrInvalidOptions, wantMsg: "Group is empty",
		},
		{
			name:    "no topics",
			mutate:  func(o *ConsumerOptions) { o.Topics = nil },
			wantErr: ErrInvalidOptions, wantMsg: "Topics is empty",
		},
		{
			name:    "an error policy this package does not implement",
			mutate:  func(o *ConsumerOptions) { o.ErrorPolicy = ErrorPolicy(9) },
			wantErr: ErrInvalidOptions, wantMsg: "not a policy",
		},
		{
			name:    "a topic name the broker would reject",
			mutate:  func(o *ConsumerOptions) { o.Topics = []string{TopicOddsNormalized, "not a topic"} },
			wantErr: ErrInvalidOptions, wantMsg: "Topics[1]",
		},
		{
			name:    "the embedded ClientOptions are validated too",
			mutate:  func(o *ConsumerOptions) { o.Brokers = nil },
			wantErr: ErrInvalidOptions,
		},
		{
			name:   "ErrorPolicySkip is accepted",
			mutate: func(o *ConsumerOptions) { o.ErrorPolicy = ErrorPolicySkip },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validConsumerOptions()
			tc.mutate(&opts)

			err := opts.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validate() = %v, want %v", err, tc.wantErr)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("validate() = %q, want it to mention %q", err, tc.wantMsg)
			}
		})
	}
}

// TestErrorPolicyZeroValueIsStop pins the default, which is the whole point of
// the ordering in the const block: the alternative default silently drops data,
// and a silent default that loses records is not a default anybody would choose
// deliberately.
func TestErrorPolicyZeroValueIsStop(t *testing.T) {
	t.Parallel()

	var zero ErrorPolicy
	if zero != ErrorPolicyStop {
		t.Fatalf("the zero ErrorPolicy is %v, want ErrorPolicyStop", zero)
	}
	if got := validConsumerOptions().ErrorPolicy; got != ErrorPolicyStop {
		t.Errorf("an unset ConsumerOptions.ErrorPolicy is %v, want ErrorPolicyStop", got)
	}

	for _, tc := range []struct {
		policy ErrorPolicy
		str    string
		valid  bool
	}{
		{ErrorPolicyStop, "stop", true},
		{ErrorPolicySkip, "skip", true},
		{ErrorPolicy(200), "unknown", false},
	} {
		if got := tc.policy.String(); got != tc.str {
			t.Errorf("ErrorPolicy(%d).String() = %q, want %q", tc.policy, got, tc.str)
		}
		if got := tc.policy.Valid(); got != tc.valid {
			t.Errorf("ErrorPolicy(%d).Valid() = %v, want %v", tc.policy, got, tc.valid)
		}
	}
}

// TestNewConsumerRefusesBadOptionsAndAnUnreachableCluster covers both
// constructor failure modes, and the difference between them: one never touches
// the network, the other exhausts the connect budget and says so.
func TestNewConsumerRefusesBadOptionsAndAnUnreachableCluster(t *testing.T) {
	t.Parallel()

	t.Run("bad options", func(t *testing.T) {
		t.Parallel()

		opts := validConsumerOptions()
		opts.Group = ""
		if _, err := NewConsumer(t.Context(), opts); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("NewConsumer = %v, want ErrInvalidOptions", err)
		}
	})

	t.Run("unreachable cluster", func(t *testing.T) {
		t.Parallel()

		opts := validConsumerOptions()
		opts.ClientOptions = unreachableProducerOptions().ClientOptions

		c, err := NewConsumer(t.Context(), opts)
		if c != nil {
			_ = c.Close()
		}
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("NewConsumer against an unreachable cluster = %v, want ErrUnavailable", err)
		}
	})
}

// -----------------------------------------------------------------------------
// A consumer with no cluster behind it
// -----------------------------------------------------------------------------

// testConsumer builds a Consumer whose client is real but belongs to no group
// and can reach nothing.
//
// That combination is what makes the callback assertions below possible without
// a broker: franz-go answers CommitRecords on a group-less client IMMEDIATELY
// with its own "not in a group" error, so a commit that is attempted is
// instantly observable as offset_commits_total{outcome="error"} and a commit
// that is not attempted leaves the series at zero. The difference between
// OnPartitionsRevoked and OnPartitionsLost is exactly that difference.
func testConsumer(t *testing.T) (*Consumer, *prometheus.Registry) {
	t.Helper()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	cl, err := kgo.NewClient(
		kgo.SeedBrokers("127.0.0.1:1"),
		kgo.WithLogger(&slogAdapter{log: discardLogger(), level: kgo.LogLevelError}),
		kgo.DialTimeout(100*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("build the test client: %v", err)
	}
	t.Cleanup(cl.Close)

	c := &Consumer{
		cl:             cl,
		adm:            kadm.NewClient(cl),
		group:          "sharpline-test",
		topics:         []string{TopicOddsNormalized},
		commitTimeout:  500 * time.Millisecond,
		lagInterval:    10 * time.Millisecond,
		maxPollRecords: DefaultMaxPollRecords,
		log:            discardLogger().With(slog.String("component", "kafka.consumer")),
		m:              m,
		tracer:         ClientOptions{}.tracer(),
		prop:           ClientOptions{}.propagator(),
		state:          make(map[topicPartition]*partitionState),
		assigned:       make(map[string]map[int32]struct{}),
	}
	c.healthChecker = healthChecker{cl: cl, m: m, log: c.log, timeout: 100 * time.Millisecond, closed: &c.closed}
	return c, reg
}

func testRecordAt(topic string, partition int32, offset int64) *kgo.Record {
	return &kgo.Record{
		Topic:     topic,
		Partition: partition,
		Offset:    offset,
		Key:       []byte("mkt-1"),
		Timestamp: time.Date(2026, 8, 17, 9, 16, 0, 0, time.UTC),
	}
}

// TestMarkTracksTheNewestHandledRecordPerPartition covers the bookkeeping the
// whole commit boundary rests on.
//
// The out-of-order case is the one worth pinning. Records arrive in offset order
// within a partition, so a rewind should be impossible — but a rewound commit
// REPLAYS everything between, which is destructive enough to be worth one
// comparison, and this is that comparison stated as a test.
func TestMarkTracksTheNewestHandledRecordPerPartition(t *testing.T) {
	t.Parallel()

	c, _ := testConsumer(t)

	c.mark(testRecordAt("a", 0, 1))
	c.mark(testRecordAt("a", 0, 5))
	c.mark(testRecordAt("a", 0, 3)) // must not rewind
	c.mark(testRecordAt("a", 1, 2))
	c.mark(testRecordAt("b", 0, 9))

	pending := c.pendingRecords()
	if len(pending) != 3 {
		t.Fatalf("pendingRecords returned %d records, want one per marked partition (3)", len(pending))
	}

	// Sorted by topic then partition, so the log line and the commit request are
	// deterministic.
	want := []struct {
		topic     string
		partition int32
		offset    int64
	}{
		{"a", 0, 5},
		{"a", 1, 2},
		{"b", 0, 9},
	}
	for i, w := range want {
		got := pending[i]
		if got.Topic != w.topic || got.Partition != w.partition || got.Offset != w.offset {
			t.Errorf("pending[%d] = %s/%d offset %d, want %s/%d offset %d",
				i, got.Topic, got.Partition, got.Offset, w.topic, w.partition, w.offset)
		}
	}
}

// TestConfirmCommittedClearsPendingAndStampsTheLagClock covers what happens
// after a successful commit.
//
// The `committed` flag is not bookkeeping for its own sake: the lag_seconds
// gauge stays ABSENT until it is true, because with no committed record there is
// no instant to measure an age from, and reporting 0 would claim perfect
// freshness for a partition nothing has processed.
func TestConfirmCommittedClearsPendingAndStampsTheLagClock(t *testing.T) {
	t.Parallel()

	c, _ := testConsumer(t)

	rec := testRecordAt("a", 0, 5)
	c.mark(rec)
	c.mark(testRecordAt("b", 0, 1))

	c.confirmCommitted([]*kgo.Record{rec})

	c.mu.Lock()
	st := c.state[topicPartition{topic: "a", partition: 0}]
	other := c.state[topicPartition{topic: "b", partition: 0}]
	c.mu.Unlock()

	if st.pending != nil {
		t.Errorf("the committed partition still has a pending record at offset %d", st.pending.Offset)
	}
	if !st.committed {
		t.Error("the committed partition is not marked committed; lag_seconds would stay absent for ever")
	}
	if !st.committedTime.Equal(rec.Timestamp) {
		t.Errorf("committedTime = %v, want the record's own timestamp %v", st.committedTime, rec.Timestamp)
	}
	if other.pending == nil {
		t.Error("a partition that was not part of the commit lost its pending record")
	}

	// A confirmation for a partition that was revoked in the meantime is a
	// no-op rather than a panic.
	c.confirmCommitted([]*kgo.Record{testRecordAt("gone", 7, 1)})
}

// TestCommitPendingWithNothingPendingIsANoOp pins the early return. It matters
// because commitPending runs on every batch, on every revoke and on every
// shutdown, and an empty commit request against the coordinator would be pure
// cost.
func TestCommitPendingWithNothingPendingIsANoOp(t *testing.T) {
	t.Parallel()

	c, reg := testConsumer(t)

	if err := c.commitPending(t.Context(), commitReasonBatch); err != nil {
		t.Fatalf("commitPending with nothing pending = %v, want nil", err)
	}
	if n := counterTotal(t, reg, "sharpline_kafka_offset_commits_total", nil); n != 0 {
		t.Errorf("a no-op commit recorded %v commits, want 0", n)
	}
}

// TestOnPartitionsLostCommitsNothingWhileOnPartitionsRevokedDoes is the test the
// two-callback design exists for.
//
// consumer.go states the bug it is avoiding: on LOST "the partitions already
// belong to another member. A commit against a stale generation either fails
// outright or, if the coordinator accepts it, moves the group's offset backwards
// over work the new owner has already done — silently duplicating or skipping
// records depending on which way it lands. Treating LOST as REVOKED is the
// classic bug this callback exists to not have."
//
// Conflating them is a one-line edit, and nothing else in the suite would notice
// it: a revoke-shaped commit on a lost generation still leaves the consumer
// running and the counts plausible. So the assertion is made directly on whether
// a commit was ATTEMPTED — which is what the two callbacks actually differ on,
// and which is observable here without a broker because a group-less client
// answers a commit instantly.
func TestOnPartitionsLostCommitsNothingWhileOnPartitionsRevokedDoes(t *testing.T) {
	t.Parallel()

	const commits = "sharpline_kafka_offset_commits_total"

	t.Run("revoked commits", func(t *testing.T) {
		t.Parallel()

		c, reg := testConsumer(t)
		c.onAssigned(t.Context(), nil, map[string][]int32{TopicOddsNormalized: {0, 1}})
		c.mark(testRecordAt(TopicOddsNormalized, 0, 4))

		c.onRevoked(t.Context(), nil, map[string][]int32{TopicOddsNormalized: {0}})

		if n := counterTotal(t, reg, commits, nil); n != 1 {
			t.Fatalf("OnPartitionsRevoked attempted %v commits, want exactly 1; the partitions are "+
				"still owned for the duration of the callback and that is the only moment progress "+
				"can be handed over cleanly", n)
		}
		if n := counterTotal(t, reg, "sharpline_kafka_partitions_revoked_total", nil); n != 1 {
			t.Errorf("revoked partitions counter = %v, want 1", n)
		}
	})

	t.Run("lost commits nothing", func(t *testing.T) {
		t.Parallel()

		c, reg := testConsumer(t)
		c.onAssigned(t.Context(), nil, map[string][]int32{TopicOddsNormalized: {0, 1}})
		c.mark(testRecordAt(TopicOddsNormalized, 0, 4))

		c.onLost(t.Context(), nil, map[string][]int32{TopicOddsNormalized: {0}})

		if n := counterTotal(t, reg, commits, nil); n != 0 {
			t.Fatalf("OnPartitionsLost attempted %v commits, want 0; the generation is fenced and a "+
				"commit would clobber the member that owns the partition now", n)
		}
		if n := counterTotal(t, reg, "sharpline_kafka_partitions_lost_total", nil); n != 1 {
			t.Errorf("lost partitions counter = %v, want 1", n)
		}
	})

	// Both callbacks forget the partition, and only the assignment callback
	// counts a rebalance — a rebalance that revokes and then assigns would
	// otherwise increment the counter twice and make KafkaRebalanceStorm fire on
	// a stable group.
	t.Run("both forget, only assignment counts a rebalance", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name string
			give func(*Consumer)
		}{
			{"revoked", func(c *Consumer) {
				c.onRevoked(context.Background(), nil, map[string][]int32{TopicOddsNormalized: {0}})
			}},
			{"lost", func(c *Consumer) {
				c.onLost(context.Background(), nil, map[string][]int32{TopicOddsNormalized: {0}})
			}},
		} {
			c, reg := testConsumer(t)
			c.onAssigned(context.Background(), nil, map[string][]int32{TopicOddsNormalized: {0, 1}})
			c.mark(testRecordAt(TopicOddsNormalized, 0, 4))
			tc.give(c)

			if got := len(c.pendingRecords()); got != 0 {
				t.Errorf("%s: %d pending records survive for a partition this member no longer owns", tc.name, got)
			}
			c.mu.Lock()
			counts := c.assignedCountsLocked()
			c.mu.Unlock()
			if counts[TopicOddsNormalized] != 1 {
				t.Errorf("%s: the assignment gauge says %d partitions, want 1", tc.name, counts[TopicOddsNormalized])
			}
			if n := counterTotal(t, reg, "sharpline_kafka_consumer_group_rebalances_total", nil); n != 1 {
				t.Errorf("%s: rebalance counter = %v after one assignment and one %s, want 1", tc.name, n, tc.name)
			}
		}
	})
}

// TestOwnedStatesReportsEveryAssignedPartition covers the snapshot the lag
// refresher takes under the lock.
//
// A partition that is assigned but has never been marked must still appear, with
// a zero state: lag_records is meaningful for it (it is how far behind the
// member is on a partition it has not started) even though lag_seconds is not.
func TestOwnedStatesReportsEveryAssignedPartition(t *testing.T) {
	t.Parallel()

	c, _ := testConsumer(t)
	c.onAssigned(t.Context(), nil, map[string][]int32{TopicOddsNormalized: {0, 1}})

	rec := testRecordAt(TopicOddsNormalized, 0, 4)
	c.mark(rec)
	c.confirmCommitted([]*kgo.Record{rec})

	owned := c.ownedStates()
	if len(owned) != 2 {
		t.Fatalf("ownedStates returned %d partitions, want the 2 that are assigned", len(owned))
	}
	if st := owned[topicPartition{TopicOddsNormalized, 0}]; !st.committed {
		t.Error("the committed partition does not report committed")
	}
	if st := owned[topicPartition{TopicOddsNormalized, 1}]; st.committed {
		t.Error("a partition this member never committed on reports committed; lag_seconds would " +
			"claim perfect freshness for a partition nothing has processed")
	}
}

// TestLagRefreshFailureLeavesTheGaugesStaleAndCountsIt covers the refresher's
// failure path.
//
// The gauges keep their previous values on purpose — stale, not wrong — and the
// counter is the only thing that makes that staleness detectable. This drives it
// against a coordinator that cannot be reached, which is the real shape of the
// failure.
func TestLagRefreshFailureLeavesTheGaugesStaleAndCountsIt(t *testing.T) {
	t.Parallel()

	c, reg := testConsumer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	c.refreshLag(ctx)

	if n := counterTotal(t, reg, "sharpline_kafka_lag_refresh_errors_total", nil); n < 1 {
		t.Fatalf("a failed lag refresh recorded %v errors, want at least 1; without the counter the "+
			"gauges are silently stale", n)
	}
}

// TestRunLagRefresherStopsWithItsContext pins the shutdown path: the refresher
// performs its first refresh immediately — a member that has just restarted
// otherwise leaves a hole in the graph for exactly the window an operator is
// staring at it — and then returns when its context is done.
func TestRunLagRefresherStopsWithItsContext(t *testing.T) {
	t.Parallel()

	c, reg := testConsumer(t)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.runLagRefresher(ctx)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLagRefresher did not return when its context was cancelled")
	}

	if n := counterTotal(t, reg, "sharpline_kafka_lag_refresh_errors_total", nil); n < 1 {
		t.Error("the refresher never performed its immediate first refresh")
	}
}

// TestRunGuards covers the two refusals Run makes before it joins anything.
func TestRunGuards(t *testing.T) {
	t.Parallel()

	t.Run("a nil handler", func(t *testing.T) {
		t.Parallel()

		c, _ := testConsumer(t)
		if err := c.Run(t.Context(), nil); !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("Run with a nil Handler = %v, want ErrInvalidOptions", err)
		}
	})

	t.Run("a closed consumer", func(t *testing.T) {
		t.Parallel()

		c, _ := testConsumer(t)
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		// Idempotent, and safe to call from a goroutine racing a publish.
		if err := c.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}

		h := HandlerFunc(func(context.Context, *Delivery) error { return nil })
		if err := c.Run(t.Context(), h); !errors.Is(err, ErrClosed) {
			t.Fatalf("Run on a closed consumer = %v, want ErrClosed", err)
		}
		if err := c.Check(t.Context()); !errors.Is(err, ErrClosed) {
			t.Fatalf("Check on a closed consumer = %v, want ErrClosed", err)
		}
		if err := c.Ping(t.Context()); !errors.Is(err, ErrClosed) {
			t.Fatalf("Ping on a closed consumer = %v, want ErrClosed", err)
		}
	})
}

// TestConsumerAccessorsCopyTheirState pins that Topics() hands back a copy: a
// caller that mutated the returned slice would silently change what this member
// believes it is subscribed to.
func TestConsumerAccessorsCopyTheirState(t *testing.T) {
	t.Parallel()

	c, _ := testConsumer(t)
	if c.Group() != "sharpline-test" {
		t.Errorf("Group() = %q", c.Group())
	}

	topics := c.Topics()
	topics[0] = "clobbered"
	if c.Topics()[0] != TopicOddsNormalized {
		t.Error("Topics() returns the consumer's own slice; a caller can rewrite the subscription list")
	}
}

// TestHandlerFuncAdaptsAFunction covers the adapter, including that an error
// from the function reaches the caller unchanged — the consume loop's whole
// error policy is built on that error surviving.
func TestHandlerFuncAdaptsAFunction(t *testing.T) {
	t.Parallel()

	want := errors.New("handler said no")
	var got *Delivery

	h := HandlerFunc(func(_ context.Context, d *Delivery) error {
		got = d
		return want
	})

	d := &Delivery{Topic: TopicOddsNormalized, Key: "mkt-1"}
	if err := h.HandleMessage(t.Context(), d); !errors.Is(err, want) {
		t.Fatalf("HandleMessage = %v, want %v", err, want)
	}
	if got != d {
		t.Error("the adapter did not pass the Delivery through")
	}
}

// TestConsumeSpanAttrs covers both shapes a consumed record can take, because
// they carry different attributes: a tombstone has no envelope to describe, so
// it reports its reason instead of a message type and version.
func TestConsumeSpanAttrs(t *testing.T) {
	t.Parallel()

	c, _ := testConsumer(t)

	value := c.consumeSpanAttrs(&Delivery{
		Topic: TopicOddsNormalized, Partition: 2, Offset: 7, Key: "mkt-1",
		Envelope: Envelope{Version: EnvelopeVersion, Type: "odds.normalized.v1"},
	})
	attrs := map[string]string{}
	for _, kv := range value {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[attrMessageType] != "odds.normalized.v1" {
		t.Errorf("%s = %q, want the envelope's type", attrMessageType, attrs[attrMessageType])
	}
	if attrs[attrMessagingGroup] != c.group {
		t.Errorf("%s = %q, want %q", attrMessagingGroup, attrs[attrMessagingGroup], c.group)
	}
	if _, ok := attrs[attrTombstone]; ok {
		t.Error("a value record's span claims to be a tombstone")
	}

	tomb := c.consumeSpanAttrs(&Delivery{
		Topic: TopicOddsNormalized, Key: "mkt-1",
		Tombstone: true, TombstoneReason: "market suspended",
	})
	attrs = map[string]string{}
	for _, kv := range tomb {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs[attrTombstone] != "true" {
		t.Errorf("%s = %q, want true", attrTombstone, attrs[attrTombstone])
	}
	if attrs[attrTombstoneReason] != "market suspended" {
		t.Errorf("%s = %q, want the stated reason", attrTombstoneReason, attrs[attrTombstoneReason])
	}
	if _, ok := attrs[attrMessageType]; ok {
		t.Error("a tombstone's span carries a message type; it has no envelope to take one from")
	}
}

// -----------------------------------------------------------------------------
// Snapshot reads
// -----------------------------------------------------------------------------

// TestSnapshotOptionsResolveTopic covers the compaction guard, which is the
// reason a Snapshotter cannot be pointed at an arbitrary topic.
//
// A from-the-start read of a retention-based topic is not a snapshot; it is
// whatever the retention window happens to still hold. The registry knows the
// cleanup policy of the declared topics and of nothing else, so an unrecognised
// name needs the caller to assert it knows better.
func TestSnapshotOptionsResolveTopic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		topic      string
		unregOK    bool
		wantErr    error
		wantTopic  string
		wantCompac bool
	}{
		{name: "odds.normalized", topic: TopicOddsNormalized, wantTopic: TopicOddsNormalized, wantCompac: true},
		{name: "price.computed", topic: TopicPriceComputed, wantTopic: TopicPriceComputed, wantCompac: true},
		{name: "wager.events is retention-based", topic: TopicWagerEvents, wantErr: ErrNotCompacted},
		{name: "a raw topic is retention-based", topic: TopicOddsRawPrefix + "synthetic", wantErr: ErrNotCompacted},
		{name: "an unregistered topic without permission", topic: "sharpline-it-throwaway", wantErr: ErrNotCompacted},
		{
			name: "an unregistered topic with permission", topic: "sharpline-it-throwaway",
			unregOK: true, wantTopic: "sharpline-it-throwaway",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := SnapshotOptions{ClientOptions: validOptions(), Topic: tc.topic, AllowUnregistered: tc.unregOK}
			if err := opts.validate(); err != nil {
				t.Fatalf("validate: %v", err)
			}

			topic, err := opts.resolveTopic()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolveTopic() = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTopic() = %v, want a topic", err)
			}
			if topic.Name() != tc.wantTopic {
				t.Errorf("resolveTopic().Name() = %q, want %q", topic.Name(), tc.wantTopic)
			}
			if topic.Compacted() != tc.wantCompac {
				t.Errorf("Compacted() = %v, want %v", topic.Compacted(), tc.wantCompac)
			}
		})
	}
}

// TestSnapshotOptionsValidate covers the shape checks that run before the
// compaction guard.
func TestSnapshotOptionsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*SnapshotOptions)
		wantErr error
	}{
		{name: "valid", mutate: func(*SnapshotOptions) {}},
		{
			name:    "no topic",
			mutate:  func(o *SnapshotOptions) { o.Topic = "" },
			wantErr: ErrInvalidTopic,
		},
		{
			name:    "a topic name the broker would reject",
			mutate:  func(o *SnapshotOptions) { o.Topic = "not a topic" },
			wantErr: ErrInvalidTopic,
		},
		{
			name:    "a negative timeout",
			mutate:  func(o *SnapshotOptions) { o.Timeout = -time.Second },
			wantErr: ErrInvalidOptions,
		},
		{
			name:    "the embedded ClientOptions are validated too",
			mutate:  func(o *SnapshotOptions) { o.Service = "" },
			wantErr: ErrInvalidOptions,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := SnapshotOptions{ClientOptions: validOptions(), Topic: TopicOddsNormalized}
			tc.mutate(&opts)

			err := opts.validate()
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestNewSnapshotterRefusesBeforeItDials proves the compaction guard runs on the
// OPTIONS rather than on the cluster: a retention-based topic is refused even
// when the brokers are unreachable, so the error names the real problem.
func TestNewSnapshotterRefusesBeforeItDials(t *testing.T) {
	t.Parallel()

	opts := SnapshotOptions{ClientOptions: unreachableProducerOptions().ClientOptions, Topic: TopicWagerEvents}
	s, err := NewSnapshotter(t.Context(), opts)
	if s != nil {
		_ = s.Close()
	}
	if !errors.Is(err, ErrNotCompacted) {
		t.Fatalf("NewSnapshotter on wager.events = %v, want ErrNotCompacted", err)
	}

	opts.Topic = TopicOddsNormalized
	s, err = NewSnapshotter(t.Context(), opts)
	if s != nil {
		_ = s.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewSnapshotter against an unreachable cluster = %v, want ErrUnavailable", err)
	}
}

// TestSnapshotterReadGuards covers the two refusals Read makes before any
// offsets are listed.
func TestSnapshotterReadGuards(t *testing.T) {
	t.Parallel()

	opts := unreachableProducerOptions().ClientOptions
	opts.SkipStartupProbe = true

	s, err := NewSnapshotter(t.Context(), SnapshotOptions{ClientOptions: opts, Topic: TopicOddsNormalized})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}

	if s.Topic().Name() != TopicOddsNormalized {
		t.Errorf("Topic() = %q, want %q", s.Topic().Name(), TopicOddsNormalized)
	}
	if _, err := s.Read(t.Context(), nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Read with a nil fn = %v, want ErrInvalidOptions", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := s.Read(t.Context(), func(context.Context, *Delivery) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("Read on a closed snapshotter = %v, want ErrClosed", err)
	}
	if _, _, err := s.Snapshot(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot on a closed snapshotter = %v, want ErrClosed", err)
	}
	if err := s.Check(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check on a closed snapshotter = %v, want ErrClosed", err)
	}
}

// TestSnapshotCompletionArithmetic covers the termination rule.
//
// The empty-and-fully-deleted cases are the point. A partition whose entire log
// has been removed by retention has end > 0 and no records to read; without the
// START offsets it would wait for a record that is never coming, and the whole
// read would hang until its deadline.
func TestSnapshotCompletionArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		next     map[int32]int64
		target   map[int32]int64
		complete bool
		done     int
	}{
		{
			name: "an empty topic is complete immediately",
			next: map[int32]int64{0: 0, 1: 0}, target: map[int32]int64{0: 0, 1: 0},
			complete: true, done: 2,
		},
		{
			name: "a partition whose log was fully deleted starts at its end",
			next: map[int32]int64{0: 400}, target: map[int32]int64{0: 400},
			complete: true, done: 1,
		},
		{
			name: "one partition behind",
			next: map[int32]int64{0: 10, 1: 3}, target: map[int32]int64{0: 10, 1: 7},
			complete: false, done: 1,
		},
		{
			name: "a partition that overshot its target still counts",
			next: map[int32]int64{0: 12}, target: map[int32]int64{0: 10},
			complete: true, done: 1,
		},
		{
			name: "an unread partition",
			next: map[int32]int64{}, target: map[int32]int64{0: 1},
			complete: false, done: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := snapshotComplete(tc.next, tc.target); got != tc.complete {
				t.Errorf("snapshotComplete = %v, want %v", got, tc.complete)
			}
			if got := completeCount(tc.next, tc.target); got != tc.done {
				t.Errorf("completeCount = %d, want %d", got, tc.done)
			}
		})
	}
}

// TestSnapshotStats covers the record tally and the log shape. Tombstones are
// counted separately from values because on a topic that has been running a
// while, zero tombstones means nothing has ever been deleted — not that
// deletions were missed.
func TestSnapshotStats(t *testing.T) {
	t.Parallel()

	s := SnapshotStats{Partitions: 6, Values: 40, Tombstones: 2, Duration: 1500 * time.Millisecond}
	if got := s.Records(); got != 42 {
		t.Errorf("Records() = %d, want 42", got)
	}

	logged := s.LogValue().String()
	for _, want := range []string{"partitions=6", "values=40", "tombstones=2", "1.5s"} {
		if !strings.Contains(logged, want) {
			t.Errorf("LogValue() = %q, want it to contain %q", logged, want)
		}
	}
}
