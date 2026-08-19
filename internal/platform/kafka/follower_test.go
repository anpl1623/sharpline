package kafka

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kgo"
)

// The BROKER-FREE half of the follower.
//
// What a real broker is needed for — that two followers of one topic each see
// every record, that the replay and the live tail arrive through one loop with
// no seam, that a partition added by Terraform is picked up — is proven in
// test/integration. What is decided without a network is here: the options gate,
// the compaction guard (which deliberately runs BEFORE any dial), the offset
// arithmetic that defines "caught up", the catch-up signal and its stats, and
// the one rule that separates this type from Consumer — that during catch-up
// neither a decode failure nor a handler failure is skippable, whatever the
// ErrorPolicy says.

// validFollowerOptions is the smallest FollowerOptions that passes validation.
func validFollowerOptions() FollowerOptions {
	return FollowerOptions{
		ClientOptions: validOptions(),
		Topic:         TopicPriceComputed,
	}
}

// testFollower builds a Follower whose client is real but belongs to no group
// and can reach nothing.
//
// Nothing below polls, so the unreachable seed broker is never dialled. It is
// there because a *kgo.Client is what healthChecker, the metrics and the record
// path all take, and a fake would prove less than the real thing sitting idle.
func testFollower(t *testing.T, policy ErrorPolicy) (*Follower, *prometheus.Registry) {
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

	f := &Follower{
		adm:            kadm.NewClient(cl),
		cl:             cl,
		topic:          PriceComputed(),
		errorPolicy:    policy,
		catchUpTimeout: DefaultSnapshotTimeout,
		maxPollRecords: DefaultFollowerMaxPollRecords,
		log:            discardLogger().With(slog.String("component", "kafka.follower")),
		m:              m,
		tracer:         ClientOptions{}.tracer(),
		prop:           ClientOptions{}.propagator(),
		caughtUp:       make(chan struct{}),
	}
	f.healthChecker = healthChecker{cl: cl, m: m, log: f.log, timeout: 100 * time.Millisecond, closed: &f.closed}
	return f, reg
}

// followerValueRecord builds a record the real producer path would have written,
// so the decode under test is the decode that runs in production.
func followerValueRecord(t *testing.T, key string, offset int64) *kgo.Record {
	t.Helper()

	value, _, err := Message{
		Type:       "price.computed.v1",
		ObservedAt: time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC),
		Payload:    json.RawMessage(`{"market_id":"` + key + `"}`),
	}.encode("pricer", time.Date(2026, 8, 18, 12, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return &kgo.Record{
		Topic:     TopicPriceComputed,
		Partition: 0,
		Offset:    offset,
		Key:       []byte(key),
		Value:     value,
		Timestamp: time.Date(2026, 8, 18, 12, 0, 1, 0, time.UTC),
	}
}

// followerTombstoneRecord builds a deletion carrying the headers the real
// Tombstone API writes.
func followerTombstoneRecord(key string, offset int64) *kgo.Record {
	ts := Tombstone{Reason: "event settled", Acknowledge: AcknowledgeDeletesKeyFromSnapshot}
	return &kgo.Record{
		Topic:     TopicPriceComputed,
		Partition: 0,
		Offset:    offset,
		Key:       []byte(key),
		Value:     nil,
		Headers:   ts.headers("pricer"),
		Timestamp: time.Date(2026, 8, 18, 12, 0, 2, 0, time.UTC),
	}
}

// followerUndecodableRecord is a record whose value is not an envelope at all.
func followerUndecodableRecord(offset int64) *kgo.Record {
	return &kgo.Record{
		Topic:     TopicPriceComputed,
		Partition: 0,
		Offset:    offset,
		Key:       []byte("mkt-broken"),
		Value:     []byte("{not json"),
		Timestamp: time.Date(2026, 8, 18, 12, 0, 3, 0, time.UTC),
	}
}

// -----------------------------------------------------------------------------
// Options
// -----------------------------------------------------------------------------

// TestFollowerOptionsValidate covers the gate that runs before any client is
// built.
func TestFollowerOptionsValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*FollowerOptions)
		wantErr error
		wantMsg string
	}{
		{name: "valid", mutate: func(*FollowerOptions) {}},
		{
			name:    "no topic",
			mutate:  func(o *FollowerOptions) { o.Topic = "" },
			wantErr: ErrInvalidTopic, wantMsg: "empty name",
		},
		{
			name:    "a topic name the broker would reject",
			mutate:  func(o *FollowerOptions) { o.Topic = "not a topic" },
			wantErr: ErrInvalidTopic, wantMsg: "Kafka allows",
		},
		{
			name:    "an error policy this package does not implement",
			mutate:  func(o *FollowerOptions) { o.ErrorPolicy = ErrorPolicy(9) },
			wantErr: ErrInvalidOptions, wantMsg: "not a policy",
		},
		{
			name:    "a negative catch-up timeout",
			mutate:  func(o *FollowerOptions) { o.CatchUpTimeout = -time.Second },
			wantErr: ErrInvalidOptions, wantMsg: "CatchUpTimeout is negative",
		},
		{
			name:    "the embedded ClientOptions are validated too",
			mutate:  func(o *FollowerOptions) { o.Brokers = nil },
			wantErr: ErrInvalidOptions, wantMsg: "Brokers is empty",
		},
		{
			// Zero is not "no budget", it is DefaultSnapshotTimeout — the same
			// convention every other duration in this package follows.
			name:   "a zero catch-up timeout is the default, not an error",
			mutate: func(o *FollowerOptions) { o.CatchUpTimeout = 0 },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validFollowerOptions()
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

// TestFollowerOptionsResolveTopic covers the compaction guard.
//
// It is the same guard SnapshotOptions applies, and the reason is the same: a
// from-the-start read of a retention-based topic is not a slate, it is whatever
// the retention window happens to still hold. Following one would give `stream`
// a market set that shrinks by itself.
func TestFollowerOptionsResolveTopic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		topic     string
		allow     bool
		wantErr   error
		wantName  string
		compacted bool
	}{
		{name: "price.computed is what stream follows", topic: TopicPriceComputed,
			wantName: TopicPriceComputed, compacted: true},
		{name: "odds.normalized is followable too", topic: TopicOddsNormalized,
			wantName: TopicOddsNormalized, compacted: true},
		{name: "wager.events is retention-based", topic: TopicWagerEvents, wantErr: ErrNotCompacted},
		{name: "a raw provider topic is retention-based", topic: TopicOddsRawPrefix + "synthetic",
			wantErr: ErrNotCompacted},
		{name: "an unregistered topic is refused by default", topic: "signals.steam", wantErr: ErrNotCompacted},
		{name: "an unregistered topic is allowed on request", topic: "signals.steam", allow: true,
			wantName: "signals.steam"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			opts := validFollowerOptions()
			opts.Topic = tc.topic
			opts.AllowUnregistered = tc.allow

			got, err := opts.resolveTopic()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("resolveTopic() = (%v, %v), want %v", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTopic() = %v, want a topic", err)
			}
			if got.Name() != tc.wantName {
				t.Errorf("resolveTopic().Name() = %q, want %q", got.Name(), tc.wantName)
			}
			if got.Compacted() != tc.compacted {
				t.Errorf("resolveTopic().Compacted() = %v, want %v", got.Compacted(), tc.compacted)
			}
		})
	}
}

// TestNewFollowerRefusesBeforeItDials proves the compaction guard runs on the
// OPTIONS rather than on the cluster: a retention-based topic is refused even
// when the brokers are unreachable, so the error names the real problem instead
// of blaming the network.
func TestNewFollowerRefusesBeforeItDials(t *testing.T) {
	t.Parallel()

	opts := validFollowerOptions()
	opts.ClientOptions = unreachableProducerOptions().ClientOptions
	opts.Topic = TopicWagerEvents

	f, err := NewFollower(t.Context(), opts)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrNotCompacted) {
		t.Fatalf("NewFollower on wager.events = %v, want ErrNotCompacted", err)
	}

	opts.Topic = ""
	f, err = NewFollower(t.Context(), opts)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("NewFollower with no topic = %v, want ErrInvalidTopic", err)
	}

	opts.Topic = TopicPriceComputed
	f, err = NewFollower(t.Context(), opts)
	if f != nil {
		_ = f.Close()
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewFollower against an unreachable cluster = %v, want ErrUnavailable", err)
	}
}

// TestNewFollowerResolvesDefaults pins the three zero-value resolutions, because
// each of them is a number an operator will look for and none of them is
// discoverable from the struct.
func TestNewFollowerResolvesDefaults(t *testing.T) {
	t.Parallel()

	opts := validFollowerOptions()
	opts.ClientOptions = unreachableProducerOptions().ClientOptions
	opts.SkipStartupProbe = true

	f, err := NewFollower(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewFollower: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	if f.catchUpTimeout != DefaultSnapshotTimeout {
		t.Errorf("catchUpTimeout = %s, want %s", f.catchUpTimeout, DefaultSnapshotTimeout)
	}
	if f.maxPollRecords != DefaultFollowerMaxPollRecords {
		t.Errorf("maxPollRecords = %d, want %d", f.maxPollRecords, DefaultFollowerMaxPollRecords)
	}
	if f.errorPolicy != ErrorPolicyStop {
		t.Errorf("errorPolicy = %v, want ErrorPolicyStop", f.errorPolicy)
	}
	if f.Topic().Name() != TopicPriceComputed {
		t.Errorf("Topic().Name() = %q, want %q", f.Topic().Name(), TopicPriceComputed)
	}
	if f.Name() != checkerName {
		t.Errorf("Name() = %q, want %q", f.Name(), checkerName)
	}
	if f.HasCaughtUp() {
		t.Error("HasCaughtUp() is true before Run; a follower that has read nothing holds no slate")
	}
	if _, ok := f.CatchUpStats(); ok {
		t.Error("CatchUpStats() reports complete before Run")
	}
}

// -----------------------------------------------------------------------------
// Lifecycle guards
// -----------------------------------------------------------------------------

// TestFollowerRunGuards covers every refusal Run makes before it lists an
// offset, plus the idempotent Close and the closed-client readiness answer.
func TestFollowerRunGuards(t *testing.T) {
	t.Parallel()

	opts := validFollowerOptions()
	opts.ClientOptions = unreachableProducerOptions().ClientOptions
	opts.SkipStartupProbe = true

	f, err := NewFollower(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewFollower: %v", err)
	}

	if err := f.Run(t.Context(), nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Run with a nil handler = %v, want ErrInvalidOptions", err)
	}

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	noop := HandlerFunc(func(context.Context, *Delivery) error { return nil })
	if err := f.Run(t.Context(), noop); !errors.Is(err, ErrClosed) {
		t.Fatalf("Run on a closed follower = %v, want ErrClosed", err)
	}
	if err := f.Check(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Check on a closed follower = %v, want ErrClosed", err)
	}
	if err := f.Ping(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Ping on a closed follower = %v, want ErrClosed", err)
	}
}

// TestFollowerRunsOnlyOnce pins the refusal that keeps the type honest.
//
// A second Run would not replay: the client holds its consume position, so it
// would continue from wherever the first stopped and would close CaughtUp while
// holding only the tail. Refusing is the difference between a loud bug and a
// replica that silently serves half a slate.
func TestFollowerRunsOnlyOnce(t *testing.T) {
	t.Parallel()

	f, _ := testFollower(t, ErrorPolicyStop)
	noop := HandlerFunc(func(context.Context, *Delivery) error { return nil })

	// The first Run is stopped immediately by an already-cancelled context; what
	// matters is that it CLAIMED the follower.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.Run(ctx, noop); err != nil {
		t.Fatalf("Run with a cancelled context = %v, want nil (a cancellation is a clean stop)", err)
	}

	err := f.Run(t.Context(), noop)
	if err == nil {
		t.Fatal("a second Run was accepted; it would resume rather than replay")
	}
	if !strings.Contains(err.Error(), "already run") {
		t.Errorf("second Run = %q, want it to say the follower has already run", err)
	}
}

// -----------------------------------------------------------------------------
// Catch-up
// -----------------------------------------------------------------------------

// TestFollowerCatchUpArithmetic covers the termination rule, which is an OFFSET
// COMPARISON and never a timer.
//
// Two cases carry the design. A partition whose entire log was deleted by
// retention has end > 0 and nothing to read: without the START offsets it would
// wait for a record that is never coming and burn the whole catch-up budget. And
// a partition that has OVERSHOT its target is complete — that is the live delta
// which arrived during the replay, was delivered rather than skipped, and pushed
// the read position past where the replay was aiming.
func TestFollowerCatchUpArithmetic(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		next     map[int32]int64
		target   map[int32]int64
		complete bool
		done     int
	}{
		{
			name: "an empty topic is caught up before the first poll",
			next: map[int32]int64{0: 0, 1: 0, 2: 0}, target: map[int32]int64{0: 0, 1: 0, 2: 0},
			complete: true, done: 3,
		},
		{
			name: "a partition whose log was fully deleted starts at its end",
			next: map[int32]int64{0: 0, 1: 912}, target: map[int32]int64{0: 0, 1: 912},
			complete: true, done: 2,
		},
		{
			name: "one partition still replaying holds the whole slate back",
			next: map[int32]int64{0: 40, 1: 11}, target: map[int32]int64{0: 40, 1: 40},
			complete: false, done: 1,
		},
		{
			name: "a live delta that overshot the target still completes the partition",
			next: map[int32]int64{0: 44}, target: map[int32]int64{0: 40},
			complete: true, done: 1,
		},
		{
			name: "a partition no fetch has reached yet",
			next: map[int32]int64{}, target: map[int32]int64{0: 1, 1: 0},
			complete: false, done: 1,
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

// TestFollowerCompleteCatchUpPublishesBeforeItSignals covers the readiness gate.
//
// The ordering is the assertion worth having: a goroutine that wakes on
// CaughtUp and immediately reads CatchUpStats must not see the zero value, so
// the stats are published before the channel closes.
func TestFollowerCompleteCatchUpPublishesBeforeItSignals(t *testing.T) {
	t.Parallel()

	f, reg := testFollower(t, ErrorPolicyStop)

	select {
	case <-f.CaughtUp():
		t.Fatal("CaughtUp() was already closed before the replay finished")
	default:
	}

	woke := make(chan SnapshotStats, 1)
	go func() {
		<-f.CaughtUp()
		stats, _ := f.CatchUpStats()
		woke <- stats
	}()

	f.completeCatchUp(SnapshotStats{Partitions: 3, Values: 118, Tombstones: 4}, 1500*time.Millisecond)

	select {
	case stats := <-woke:
		if stats.Values != 118 || stats.Tombstones != 4 || stats.Partitions != 3 {
			t.Errorf("a waiter woken by CaughtUp read %+v, want the published stats", stats)
		}
		if stats.Duration != 1500*time.Millisecond {
			t.Errorf("Duration = %s, want 1.5s", stats.Duration)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CaughtUp() never closed")
	}

	if !f.HasCaughtUp() {
		t.Error("HasCaughtUp() = false after the replay completed")
	}
	stats, ok := f.CatchUpStats()
	if !ok {
		t.Fatal("CatchUpStats() reports incomplete after the replay completed")
	}
	if got := stats.Records(); got != 122 {
		t.Errorf("Records() = %d, want 122", got)
	}

	// The replay reuses the snapshot series rather than inventing a parallel
	// one: it measures the same quantity.
	for _, tc := range []struct {
		kind string
		want float64
	}{
		{snapshotKindValue, 118},
		{snapshotKindTombstone, 4},
	} {
		got := counterTotal(t, reg, "sharpline_kafka_snapshot_records_total",
			map[string]string{"topic": TopicPriceComputed, "kind": tc.kind})
		if got != tc.want {
			t.Errorf("snapshot_records_total{kind=%q} = %v, want %v", tc.kind, got, tc.want)
		}
	}

	// Idempotent: a second call must not panic on a closed channel.
	f.completeCatchUp(SnapshotStats{Partitions: 3}, time.Second)
}

// -----------------------------------------------------------------------------
// Record handling
// -----------------------------------------------------------------------------

// TestFollowerDeliversTombstones pins the contract every handler of a compacted
// topic depends on.
//
// A tombstone is delivered like any other record, with Delivery.Tombstone set
// and the producer's stated reason attached. A hub that ignored it would keep a
// dead market in its fold for ever, because no further record for that key is
// coming.
func TestFollowerDeliversTombstones(t *testing.T) {
	t.Parallel()

	f, _ := testFollower(t, ErrorPolicyStop)

	var seen []*Delivery
	h := HandlerFunc(func(_ context.Context, d *Delivery) error {
		seen = append(seen, d)
		return nil
	})

	value, err := f.handleRecord(t.Context(), h, followerValueRecord(t, "mkt-1", 4), false)
	if err != nil {
		t.Fatalf("handleRecord on a value record: %v", err)
	}
	tomb, err := f.handleRecord(t.Context(), h, followerTombstoneRecord("mkt-1", 5), false)
	if err != nil {
		t.Fatalf("handleRecord on a tombstone: %v", err)
	}

	if len(seen) != 2 {
		t.Fatalf("the handler saw %d records, want 2", len(seen))
	}
	if value == nil || value.Tombstone {
		t.Errorf("the value record was reported as %+v, want a non-tombstone Delivery", value)
	}
	if value != nil && value.Envelope.Type != "price.computed.v1" {
		t.Errorf("Envelope.Type = %q, want %q", value.Envelope.Type, "price.computed.v1")
	}
	if tomb == nil || !tomb.Tombstone {
		t.Fatalf("the tombstone was reported as %+v, want Tombstone true", tomb)
	}
	if tomb.TombstoneReason != "event settled" {
		t.Errorf("TombstoneReason = %q, want %q", tomb.TombstoneReason, "event settled")
	}
}

// TestFollowerCatchUpToleratesNothing is the rule that separates this type from
// Consumer.
//
// Snapshotter's reason, applied to a follower: "a snapshot with a hole in it is
// not a snapshot — it is a current-line view that is silently missing a market."
// While the slate is still being built, neither an undecodable record nor a
// failing handler may be skipped, whatever the ErrorPolicy says. AFTER catch-up
// the policy takes effect, because by then a dropped update is one stale market
// until its next price rather than a market that is missing entirely.
func TestFollowerCatchUpToleratesNothing(t *testing.T) {
	t.Parallel()

	failing := HandlerFunc(func(context.Context, *Delivery) error {
		return errors.New("the hub refused the market")
	})
	noop := HandlerFunc(func(context.Context, *Delivery) error { return nil })

	cases := []struct {
		name     string
		policy   ErrorPolicy
		caughtUp bool
		record   func(t *testing.T) *kgo.Record
		handler  Handler
		wantErr  error
		wantNil  bool
	}{
		{
			name:   "an undecodable record is fatal during catch-up under Skip",
			policy: ErrorPolicySkip, caughtUp: false,
			record:  func(*testing.T) *kgo.Record { return followerUndecodableRecord(1) },
			handler: noop, wantErr: ErrMalformedEnvelope,
		},
		{
			name:   "an undecodable record is fatal during catch-up under Stop",
			policy: ErrorPolicyStop, caughtUp: false,
			record:  func(*testing.T) *kgo.Record { return followerUndecodableRecord(1) },
			handler: noop, wantErr: ErrMalformedEnvelope,
		},
		{
			name:   "an undecodable record is skipped after catch-up under Skip",
			policy: ErrorPolicySkip, caughtUp: true,
			record:  func(*testing.T) *kgo.Record { return followerUndecodableRecord(1) },
			handler: noop, wantNil: true,
		},
		{
			name:   "an undecodable record still stops the loop after catch-up under Stop",
			policy: ErrorPolicyStop, caughtUp: true,
			record:  func(*testing.T) *kgo.Record { return followerUndecodableRecord(1) },
			handler: noop, wantErr: ErrMalformedEnvelope,
		},
		{
			name:   "a failing handler is fatal during catch-up under Skip",
			policy: ErrorPolicySkip, caughtUp: false,
			record:  func(t *testing.T) *kgo.Record { return followerValueRecord(t, "mkt-1", 2) },
			handler: failing, wantErr: ErrHandlerFailed,
		},
		{
			name:   "a failing handler is tolerated after catch-up under Skip",
			policy: ErrorPolicySkip, caughtUp: true,
			record:  func(t *testing.T) *kgo.Record { return followerValueRecord(t, "mkt-1", 2) },
			handler: failing,
		},
		{
			name:   "a failing handler stops the loop after catch-up under Stop",
			policy: ErrorPolicyStop, caughtUp: true,
			record:  func(t *testing.T) *kgo.Record { return followerValueRecord(t, "mkt-1", 2) },
			handler: failing, wantErr: ErrHandlerFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f, _ := testFollower(t, tc.policy)
			d, err := f.handleRecord(t.Context(), tc.handler, tc.record(t), tc.caughtUp)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("handleRecord = %v, want %v", err, tc.wantErr)
				}
				if d != nil {
					t.Errorf("handleRecord returned a Delivery alongside its error")
				}
				return
			}
			if err != nil {
				t.Fatalf("handleRecord = %v, want nil", err)
			}
			if tc.wantNil && d != nil {
				t.Errorf("handleRecord returned %+v for a skipped record, want nil so it is not tallied", d)
			}
			if !tc.wantNil && d == nil {
				t.Error("handleRecord returned no Delivery for a record the handler saw")
			}
		})
	}
}

// TestFollowerMetricsCarryNoGroup pins the label the dashboard depends on
// staying honest.
//
// A follower joins no group, so an invented group name would appear in the
// dashboard's `sum by (group, topic)` as a member that does not exist.
// Snapshotter already set this precedent; this asserts the follower follows it.
func TestFollowerMetricsCarryNoGroup(t *testing.T) {
	t.Parallel()

	if followerGroup != "" {
		t.Fatalf("followerGroup = %q, want the empty string: a follower has no group", followerGroup)
	}

	f, reg := testFollower(t, ErrorPolicyStop)
	noop := HandlerFunc(func(context.Context, *Delivery) error { return nil })

	if _, err := f.handleRecord(t.Context(), noop, followerValueRecord(t, "mkt-1", 6), true); err != nil {
		t.Fatalf("handleRecord: %v", err)
	}

	got := counterTotal(t, reg, "sharpline_kafka_consume_records_total",
		map[string]string{"group": "", "topic": TopicPriceComputed})
	if got != 1 {
		t.Errorf("consume_records_total{group=\"\",topic=%q} = %v, want 1", TopicPriceComputed, got)
	}

	// And nothing was written to either group-lag series, because there is no
	// group lag to write. A gauge here would poison two alert rules.
	for _, name := range []string{
		"sharpline_kafka_consumer_lag_records",
		"sharpline_kafka_consumer_lag_seconds",
		"sharpline_kafka_offset_commits_total",
	} {
		families, err := reg.Gather()
		if err != nil {
			t.Fatalf("gather: %v", err)
		}
		for _, fam := range families {
			if fam.GetName() == name && len(fam.GetMetric()) > 0 {
				t.Errorf("%s has samples; a follower commits nothing and has no group lag", name)
			}
		}
	}
}

// -----------------------------------------------------------------------------
// Waiting for a topic that does not exist yet
// -----------------------------------------------------------------------------

// TestIsTopicNotCreatedYet pins the classification that decides between waiting
// and failing.
//
// It matters more than a predicate usually does. Classify this too broadly and a
// real fault — an authorization failure, an unreachable cluster — becomes a
// silent forever-wait with a red readiness probe and no stated cause. Classify
// it too narrowly and `stream` goes back to the behaviour the live gate caught:
// the hub stops permanently the first time it starts before Terraform has
// applied, and the topic appearing afterwards fixes nothing.
func TestIsTopicNotCreatedYet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not a missing topic", err: nil, want: false},
		{
			name: "the broker's UNKNOWN_TOPIC_OR_PARTITION",
			err:  kerr.UnknownTopicOrPartition,
			want: true,
		},
		{
			name: "UNKNOWN_TOPIC_OR_PARTITION wrapped in this package's context",
			err: fmt.Errorf("kafka: list end offsets for price.computed: %w",
				kerr.UnknownTopicOrPartition),
			want: true,
		},
		{
			name: "a listing that described no partitions",
			err:  fmt.Errorf("%w: price.computed has no partitions", ErrInvalidTopic),
			want: true,
		},
		{
			// The failure mode this rules out: waiting for ever on a topic the
			// cluster will never let us read.
			name: "TOPIC_AUTHORIZATION_FAILED is a real fault, not a wait",
			err:  kerr.TopicAuthorizationFailed,
			want: false,
		},
		{
			name: "LEADER_NOT_AVAILABLE is franz-go's to retry, not ours to wait on",
			err:  kerr.LeaderNotAvailable,
			want: false,
		},
		{
			name: "a plain error is not a missing topic",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "a closed client is not a missing topic",
			err:  ErrClosed,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTopicNotCreatedYet(tt.err); got != tt.want {
				t.Fatalf("isTopicNotCreatedYet(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestAwaitCatchUpTargetsStopsOnCallerCancellation proves the wait is bounded by
// the caller and not only by the topic appearing.
//
// The wait is deliberately unbounded in duration — a declared topic that does
// not exist yet is an ordinary startup state and there is no honest deadline for
// "has an operator applied Terraform". What must NOT be unbounded is the
// process's ability to shut down: a SIGTERM during that wait has to end it, or a
// replica that started before its topics would ignore its grace period and be
// SIGKILLed.
//
// It uses a Follower whose admin client points at nothing, so every listing
// fails with a dial error rather than with UNKNOWN_TOPIC_OR_PARTITION — which
// exercises the OTHER branch and asserts the complementary property: a fault
// that is not a missing topic is returned immediately rather than waited on.
func TestAwaitCatchUpTargetsStopsOnCallerCancellation(t *testing.T) {
	t.Parallel()

	opts := validFollowerOptions()
	opts.ClientOptions = unreachableProducerOptions().ClientOptions
	// The constructor's connectivity gate would refuse these brokers before Run
	// was ever reached, and the gate is not what this test is about.
	opts.SkipStartupProbe = true
	opts.Topic = TopicPriceComputed

	f, err := NewFollower(t.Context(), opts)
	if err != nil {
		t.Fatalf("NewFollower = %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		_, _, err := f.awaitCatchUpTargets(ctx, ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("awaitCatchUpTargets returned nil for a cancelled caller")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("awaitCatchUpTargets did not return on a cancelled context; a replica that " +
			"started before its topics existed would ignore SIGTERM and be killed")
	}
}
