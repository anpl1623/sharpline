// The hub: the snapshot-then-delta contract, the fanout, backpressure
// isolation, the readiness gate and the shutdown drain.
//
// Every record these tests fold is a real pricing.ComputedMarket — built by
// state_test.go's fixture through the real types, validated by the document's
// own validator, and carried in the envelope the bus would carry it in. Nothing
// here hand-writes a payload or invents a price. server_test.go goes one step
// further and drives the actual synthetic provider through the actual
// normalizer and the actual pricing engine.
package wsgw

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/pricing"
)

// -----------------------------------------------------------------------------
// Test doubles and helpers
// -----------------------------------------------------------------------------

// stubSource is a [BusSource] that replays a prepared run of deliveries and
// then holds the loop open, which is exactly the shape of a real follower: the
// replay and the live tail are one uninterrupted stream (D1).
type stubSource struct {
	records []*kafka.Delivery
	caught  atomic.Bool
	replay  chan struct{}
	once    sync.Once
}

func newStubSource(records ...*kafka.Delivery) *stubSource {
	return &stubSource{records: records, replay: make(chan struct{})}
}

func (s *stubSource) Run(ctx context.Context, h kafka.Handler) error {
	for _, d := range s.records {
		if err := h.HandleMessage(ctx, d); err != nil {
			return err
		}
	}
	s.caught.Store(true)
	s.once.Do(func() { close(s.replay) })
	<-ctx.Done()
	return nil
}

func (s *stubSource) HasCaughtUp() bool { return s.caught.Load() }

// countingPresence records every call and can be made to fail, so the
// degradation contract (D6) is asserted rather than assumed.
type countingPresence struct {
	mu       sync.Mutex
	calls    map[string]int
	channels []string
	fail     error
}

func newCountingPresence() *countingPresence {
	return &countingPresence{calls: make(map[string]int)}
}

func (p *countingPresence) record(op string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[op]++
	return p.fail
}

func (p *countingPresence) count(op string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[op]
}

func (p *countingPresence) Channels(context.Context, string) ([]string, error) {
	if err := p.record("channels"); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.channels...), nil
}

func (p *countingPresence) Connected(context.Context, string, string, bool) error {
	return p.record("connected")
}

func (p *countingPresence) Subscribe(context.Context, string, []string, string, bool) error {
	return p.record("subscribe")
}

func (p *countingPresence) Unsubscribe(context.Context, string, []string) error {
	return p.record("unsubscribe")
}

func (p *countingPresence) Touch(context.Context, string, string) error { return p.record("touch") }

func (p *countingPresence) Disconnected(context.Context, string, string) error {
	return p.record("disconnected")
}

// newTestHub builds a hub with a fresh registry and no bus.
func newTestHub(t *testing.T, mutate func(*HubOptions)) (*Hub, *prometheus.Registry) {
	t.Helper()
	m, reg := testMetrics(t)
	opts := HubOptions{
		Options: Options{Logger: testLogger(), Metrics: m},
		Clock:   time.Now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	h, err := NewHub(opts)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	return h, reg
}

// joinHub builds a connection against a hub and registers it, exactly as the
// upgrade handler would. It starts no goroutine: most assertions here read the
// send queue directly, because what is being tested is what the hub QUEUED and
// in what order, not what the socket did with it.
func joinHub(t *testing.T, h *Hub, sock wsConn, mutate func(*Options)) *conn {
	t.Helper()
	opts := h.Options()
	if mutate != nil {
		mutate(&opts)
		opts = opts.Normalise()
	}
	c := newConn(connOptions{
		ID:      h.newID(),
		Socket:  sock,
		Hub:     h,
		Options: opts,
		Logger:  testLogger(),
		Now:     h.now,
	})
	if err := h.Register(context.Background(), c); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return c
}

// queued drains a connection's send queue and decodes each frame.
func queued(t *testing.T, c *conn) []testFrame {
	t.Helper()
	var out []testFrame
	for {
		select {
		case o := <-c.send:
			out = append(out, decodeTestFrame(t, o.payload))
		default:
			return out
		}
	}
}

// testFrame is the decoded shape of every server frame, flattened so one type
// serves every assertion in this file.
type testFrame struct {
	Seq      uint64            `json:"seq"`
	Type     MessageKind       `json:"type"`
	ID       string            `json:"id"`
	Channel  string            `json:"channel"`
	Markets  []json.RawMessage `json:"markets"`
	Market   json.RawMessage   `json:"market"`
	Removed  string            `json:"removed"`
	Complete bool              `json:"complete"`
	Reason   string            `json:"reason"`
	Code     ErrorCode         `json:"code"`
	Channels []string          `json:"channels"`
	Resumed  bool              `json:"resumed"`
	Rejected []RejectedChannel `json:"rejected"`
}

func decodeTestFrame(t *testing.T, raw []byte) testFrame {
	t.Helper()
	var f testFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("frame %q is not JSON: %v", raw, err)
	}
	return f
}

// kinds renders a run of frames as their kinds, for a readable failure.
func kinds(frames []testFrame) []MessageKind {
	out := make([]MessageKind, 0, len(frames))
	for _, f := range frames {
		out = append(out, f.Type)
	}
	return out
}

// fingerprint reads the version marker off a carried market document. It goes
// through the real type, so a payload that stopped being a ComputedMarket fails
// here rather than silently comparing empty strings.
func fingerprint(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var c pricing.ComputedMarket
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("carried payload is not a ComputedMarket: %v", err)
	}
	return c.SourceFingerprint
}

// versioned builds a delivery of the fixture market at version i: a distinct
// fingerprint and a strictly later observation instant, so the slate's
// monotonicity guard admits it.
func versioned(t *testing.T, i int) *kafka.Delivery {
	t.Helper()
	c := sampleComputed(t)
	c.SourceFingerprint = strconv.Itoa(i)
	c.ObservedAt = sampleObservedAt.Add(time.Duration(i) * time.Millisecond)
	c.IngestedAt = c.ObservedAt.Add(500 * time.Millisecond)
	for bi := range c.Books {
		for qi := range c.Books[bi].Quotes {
			c.Books[bi].Quotes[qi].ObservedAt = c.ObservedAt
		}
	}
	return delivery(t, c, int64(i))
}

// histogram reads one histogram child's sample count by label values.
func histogram(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) uint64 {
	t.Helper()
	f := gather(t, reg)[name]
	if f == nil {
		return 0
	}
	var total uint64
	for _, m := range f.GetMetric() {
		if matchesLabels(m, want) {
			total += m.GetHistogram().GetSampleCount()
		}
	}
	return total
}

func matchesLabels(m *dto.Metric, want map[string]string) bool {
	got := labelSet(m)
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// Snapshot then delta (D2)
// -----------------------------------------------------------------------------

func TestSubscribeAnswersWithAnAckThenASnapshotThenDeltas(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	ctx := context.Background()

	// A market exists on the slate before anybody subscribes, which is the
	// normal state: the compacted replay ran at startup.
	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	c := joinHub(t, h, newFakeSocket(), nil)
	h.subscribe(ctx, c, []string{"market:" + sampleMarketID}, "req-1")

	// And a change afterwards.
	if err := h.HandleMessage(ctx, versioned(t, 2)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	frames := queued(t, c)
	want := []MessageKind{KindHello, KindAck, KindSnapshot, KindDelta}
	if got := kinds(frames); len(got) != len(want) {
		t.Fatalf("frames are %v, want %v", got, want)
	}
	for i, k := range want {
		if frames[i].Type != k {
			t.Fatalf("frames are %v, want %v", kinds(frames), want)
		}
		if frames[i].Seq != uint64(i+1) {
			t.Fatalf("frame %d carries seq %d, want %d", i, frames[i].Seq, i+1)
		}
	}

	ack := frames[1]
	if ack.ID != "req-1" {
		t.Fatalf("the ack echoes id %q, want req-1", ack.ID)
	}
	if len(ack.Rejected) != 0 {
		t.Fatalf("the ack rejected %v", ack.Rejected)
	}

	snap := frames[2]
	if !snap.Complete {
		t.Fatal("the snapshot is not marked complete")
	}
	if len(snap.Markets) != 1 {
		t.Fatalf("the snapshot carries %d markets, want 1", len(snap.Markets))
	}
	if got := fingerprint(t, snap.Markets[0]); got != "1" {
		t.Fatalf("the snapshot carries version %q, want 1", got)
	}
	if got := fingerprint(t, frames[3].Market); got != "2" {
		t.Fatalf("the delta carries version %q, want 2", got)
	}
}

func TestSubscribingToAnEmptyChannelYieldsAnEmptySnapshot(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	c := joinHub(t, h, newFakeSocket(), nil)
	h.subscribe(context.Background(), c, []string{"league:nba"}, "")

	frames := queued(t, c)
	if got := kinds(frames); len(got) != 3 || frames[2].Type != KindSnapshot {
		t.Fatalf("frames are %v, want hello, ack, snapshot", got)
	}
	snap := frames[2]
	if snap.Markets == nil {
		t.Fatal("the snapshot's markets rendered as null; an empty channel must render []")
	}
	if len(snap.Markets) != 0 {
		t.Fatalf("the snapshot invented %d markets for a channel that has none", len(snap.Markets))
	}
	if !snap.Complete {
		t.Fatal("an empty snapshot must still be complete — it is a correct answer, not a partial one")
	}
}

func TestTombstoneBecomesARemovalDelta(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	ctx := context.Background()

	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
	c := joinHub(t, h, newFakeSocket(), nil)
	h.subscribe(ctx, c, []string{"event:" + sampleEventID}, "")
	_ = queued(t, c)

	if err := h.HandleMessage(ctx, tombstone(sampleMarketID, sampleObservedAt.Add(time.Hour))); err != nil {
		t.Fatalf("HandleMessage(tombstone): %v", err)
	}

	frames := queued(t, c)
	if len(frames) != 1 || frames[0].Type != KindDelta {
		t.Fatalf("frames are %v, want one delta", kinds(frames))
	}
	if frames[0].Removed != sampleMarketID {
		t.Fatalf("the tombstone delta removes %q, want %q", frames[0].Removed, sampleMarketID)
	}
	if len(frames[0].Market) != 0 {
		t.Fatal("a tombstone delta carries a market document; exactly one of market and removed is populated")
	}
	// The tombstone reached the EVENT channel, not only market:{id}. A deletion
	// announced only on the narrow channel leaves the market on every board
	// that was showing it, for ever, because no further record for that key is
	// coming.
	if frames[0].Channel != "event:"+sampleEventID {
		t.Fatalf("the removal arrived on %q, want the event channel", frames[0].Channel)
	}
}

func TestSnapshotAndDeltaCannotInterleave(t *testing.T) {
	t.Parallel()

	// The hazard: a delta published between "the snapshot was read" and "this
	// connection is registered as a subscriber" is lost, and nothing downstream
	// can tell — the market simply shows a price that stopped moving. D2 closes
	// the window by doing both under one lock. This races the two against each
	// other and asserts the property that window would break.
	const (
		iterations = 25
		versions   = 120
	)

	for iter := 0; iter < iterations; iter++ {
		h, _ := newTestHub(t, nil)
		ctx := context.Background()

		records := make([]*kafka.Delivery, 0, versions)
		for i := 1; i <= versions; i++ {
			records = append(records, versioned(t, i))
		}

		// A queue large enough that nothing here is lost to backpressure; the
		// property under test is ordering, and a discarded buffer would be a
		// legitimate gap that would mask a real one.
		c := newConn(connOptions{
			ID:      "race",
			Socket:  newFakeSocket(),
			Hub:     h,
			Options: Options{Logger: testLogger(), Metrics: h.m, SendQueueCapacity: 4 * versions},
			Logger:  testLogger(),
			Now:     h.now,
		})

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for _, d := range records {
				_ = h.HandleMessage(ctx, d)
			}
		}()
		go func() {
			defer wg.Done()
			if err := h.Register(ctx, c); err != nil {
				return
			}
			h.subscribe(ctx, c, []string{"market:" + sampleMarketID}, "")
		}()
		wg.Wait()

		frames := queued(t, c)

		// Walk the frames and check the one invariant that matters: whatever
		// version the snapshot carried, every delta after it is the next
		// version, with no hole and no repeat, up to the last one published.
		var have int
		var seenSnapshot bool
		for _, f := range frames {
			switch f.Type {
			case KindSnapshot:
				seenSnapshot = true
				if len(f.Markets) == 0 {
					have = 0
					continue
				}
				v, err := strconv.Atoi(fingerprint(t, f.Markets[0]))
				if err != nil {
					t.Fatalf("iteration %d: snapshot version is not a number: %v", iter, err)
				}
				have = v
			case KindDelta:
				if !seenSnapshot {
					t.Fatalf("iteration %d: a delta arrived before the snapshot", iter)
				}
				v, err := strconv.Atoi(fingerprint(t, f.Market))
				if err != nil {
					t.Fatalf("iteration %d: delta version is not a number: %v", iter, err)
				}
				if v != have+1 {
					t.Fatalf("iteration %d: the client held version %d and received %d; "+
						"a delta was %s across the snapshot boundary",
						iter, have, v, lostOrDuplicated(have, v))
				}
				have = v
			default:
			}
		}
		if have != versions {
			t.Fatalf("iteration %d: the client ends at version %d, want %d — updates were "+
				"published that nothing delivered", iter, have, versions)
		}
	}
}

func lostOrDuplicated(have, got int) string {
	if got <= have {
		return "duplicated"
	}
	return fmt.Sprintf("lost (%d missing)", got-have-1)
}

// -----------------------------------------------------------------------------
// Backpressure isolation (D4) — CLAUDE.md §5's actual requirement
// -----------------------------------------------------------------------------

func TestOneStalledClientDoesNotStarveTheOthers(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The slow client: a tiny queue and a socket that has stopped reading.
	slowSock := newFakeSocket()
	slowSock.stall()
	slow := joinHub(t, h, slowSock, func(o *Options) { o.SendQueueCapacity = 4 })

	// The healthy client: a queue with room and a socket that drains.
	fastSock := newFakeSocket()
	fast := joinHub(t, h, fastSock, func(o *Options) { o.SendQueueCapacity = 1024 })

	slowDone := make(chan struct{})
	fastDone := make(chan struct{})
	go func() { defer close(slowDone); slow.serve(ctx) }()
	go func() { defer close(fastDone); fast.serve(ctx) }()

	channel := "market:" + sampleMarketID
	h.subscribe(ctx, slow, []string{channel}, "")
	h.subscribe(ctx, fast, []string{channel}, "")

	const updates = 100
	for i := 1; i <= updates; i++ {
		if err := h.HandleMessage(ctx, versioned(t, i)); err != nil {
			t.Fatalf("HandleMessage: %v", err)
		}
	}

	// The healthy client received EVERY update. This is the requirement:
	// "rather than letting one slow consumer apply backpressure to the entire
	// hub".
	waitFor(t, func() bool { return countDeltas(t, fastSock.frames()) == updates })

	got := countDeltas(t, fastSock.frames())
	if got != updates {
		t.Fatalf("the healthy client received %d deltas, want %d", got, updates)
	}

	if n := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(DropSlowConsumer)); n == 0 {
		t.Fatal("the stalled client's buffer was never discarded; the queue is not bounded")
	}
	if n := counter(t, reg, "sharpline_ws_resyncs_total", "reason", string(ResyncSlowConsumer)); n == 0 {
		t.Fatal("the stalled client was never told to resync")
	}

	// And it is the BUFFER that was dropped, not the connection: the stalled
	// client is still registered and still being served.
	if slow.closing() {
		t.Fatal("the stalled client's connection was closed; CLAUDE.md §5 drops the buffer, not the client")
	}
	if h.Connections() != 2 {
		t.Fatalf("the hub holds %d connections, want 2", h.Connections())
	}

	slowSock.resume()
	cancel()
	slow.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	fast.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-slowDone
	<-fastDone
}

func countDeltas(t *testing.T, raw [][]byte) int {
	t.Helper()
	n := 0
	for _, b := range raw {
		if decodeTestFrame(t, b).Type == KindDelta {
			n++
		}
	}
	return n
}

// -----------------------------------------------------------------------------
// The staleness observation (D7)
// -----------------------------------------------------------------------------

func TestFanoutIsObservedOncePerEventNotOncePerSubscriber(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	ctx := context.Background()

	const subscribers = 4
	conns := make([]*conn, 0, subscribers)
	for i := 0; i < subscribers; i++ {
		c := joinHub(t, h, newFakeSocket(), func(o *Options) { o.SendQueueCapacity = 64 })
		h.subscribe(ctx, c, []string{"market:" + sampleMarketID}, "")
		conns = append(conns, c)
	}

	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	// The fixture carries one book with two quotes, so staleness — which is
	// observed PER PRICE — must have exactly two samples. Four subscribers must
	// not make it eight: that would weight the headline SLO by how popular a
	// market is rather than by how old the price was.
	stale := histogram(t, reg, "sharpline_odds_staleness_seconds", map[string]string{"stage": "fanout"})
	if stale != 2 {
		t.Fatalf("staleness has %d samples, want 2 — one per price on the record, "+
			"regardless of how many clients received it", stale)
	}

	// Pipeline latency is a property of the RECORD, so exactly one sample.
	lat := histogram(t, reg, "sharpline_pipeline_latency_seconds", map[string]string{"stage": "fanout"})
	if lat != 1 {
		t.Fatalf("pipeline latency has %d samples, want 1", lat)
	}

	// But every subscriber really did receive it.
	if n := counter(t, reg, "sharpline_ws_messages_sent_total", "kind", string(KindDelta)); n != subscribers {
		t.Fatalf("messages_sent_total{delta} = %v, want %d", n, subscribers)
	}
	for i, c := range conns {
		if countQueuedDeltas(t, c) != 1 {
			t.Fatalf("subscriber %d did not receive the delta", i)
		}
	}
}

func countQueuedDeltas(t *testing.T, c *conn) int {
	t.Helper()
	n := 0
	for _, f := range queued(t, c) {
		if f.Type == KindDelta {
			n++
		}
	}
	return n
}

func TestARecordNobodyIsSubscribedToIsNotAFanoutEvent(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	if err := h.HandleMessage(context.Background(), versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	if n := histogram(t, reg, "sharpline_odds_staleness_seconds", map[string]string{"stage": "fanout"}); n != 0 {
		t.Fatalf("staleness has %d samples with no subscribers; the SLO would become a "+
			"statement about the bus rather than about the clients", n)
	}
	if n := counter(t, reg, "sharpline_ws_bus_records_total", "result", "stored"); n != 1 {
		t.Fatalf("bus_records_total{stored} = %v, want 1 — the record must still be folded", n)
	}
}

// -----------------------------------------------------------------------------
// Subscription bookkeeping
// -----------------------------------------------------------------------------

func TestSubscribeReportsPartialSuccess(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	c := joinHub(t, h, newFakeSocket(), nil)

	h.subscribe(context.Background(), c, []string{
		"market:" + sampleMarketID, // good
		"market:" + sampleMarketID, // duplicate within one frame
		"nonsense",                 // no colon
		"planet:mars",              // unknown kind
		"league:NOT A SLUG",        // bad identifier
	}, "req")

	frames := queued(t, c)
	var ack testFrame
	for _, f := range frames {
		if f.Type == KindAck {
			ack = f
		}
	}
	if ack.Type != KindAck {
		t.Fatalf("no ack among %v", kinds(frames))
	}
	if len(ack.Rejected) != 4 {
		t.Fatalf("the ack rejected %d channels, want 4: %+v", len(ack.Rejected), ack.Rejected)
	}

	byReason := map[RejectReason]int{}
	for _, r := range ack.Rejected {
		byReason[r.Reason]++
	}
	for _, want := range []RejectReason{RejectMalformed, RejectUnknownKind, RejectInvalidID, RejectDuplicate} {
		if byReason[want] != 1 {
			t.Fatalf("expected exactly one %s rejection, got %d: %+v", want, byReason[want], ack.Rejected)
		}
		if n := counter(t, reg, "sharpline_ws_channel_rejects_total", "reason", string(want)); n != 1 {
			t.Fatalf("channel_rejects_total{%s} = %v, want 1", want, n)
		}
	}

	// The one good channel really is subscribed: a partial success is reported
	// as one, never rounded to failure.
	var snapshots int
	for _, f := range frames {
		if f.Type == KindSnapshot {
			snapshots++
		}
	}
	if snapshots != 1 {
		t.Fatalf("got %d snapshots, want 1 for the single accepted channel", snapshots)
	}
}

func TestChannelLimitIsRefusedNotTruncated(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, func(o *HubOptions) { o.Options.MaxChannelsPerConnection = 2 })
	c := joinHub(t, h, newFakeSocket(), nil)

	h.subscribe(context.Background(), c, []string{
		"market:a", "market:b", "market:c", "market:d",
	}, "")

	var ack testFrame
	for _, f := range queued(t, c) {
		if f.Type == KindAck {
			ack = f
		}
	}
	if len(ack.Rejected) != 2 {
		t.Fatalf("the ack rejected %d channels, want 2: %+v", len(ack.Rejected), ack.Rejected)
	}
	for _, r := range ack.Rejected {
		if r.Reason != RejectLimitReached {
			t.Fatalf("rejection reason is %q, want %q", r.Reason, RejectLimitReached)
		}
	}
	if got := len(c.channels); got != 2 {
		t.Fatalf("the connection holds %d channels, want the cap of 2", got)
	}
}

func TestUnsubscribeStopsDeliveryAndTearsTheRouteDown(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	ctx := context.Background()

	c := joinHub(t, h, newFakeSocket(), nil)
	channel := "market:" + sampleMarketID
	h.subscribe(ctx, c, []string{channel}, "")
	_ = queued(t, c)

	h.unsubscribe(ctx, c, []string{channel}, "bye")
	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	frames := queued(t, c)
	if len(frames) != 1 || frames[0].Type != KindAck {
		t.Fatalf("frames are %v, want a single ack; a delta after an unsubscribe means "+
			"the routing entry outlived the subscription", kinds(frames))
	}

	// The bucket itself is gone, not merely empty: a map that only ever grew
	// would retain one entry per channel any client ever watched.
	var buckets int
	h.store.Exclusive(func() { buckets = len(h.subs) })
	if buckets != 0 {
		t.Fatalf("the routing table holds %d channel buckets after the last unsubscribe", buckets)
	}
}

func TestUnregisterRemovesEveryRoute(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	ctx := context.Background()

	c := joinHub(t, h, newFakeSocket(), nil)
	h.subscribe(ctx, c, []string{"market:a", "market:b", "league:nfl"}, "")

	h.unregister(ctx, c)

	var buckets, conns int
	h.store.Exclusive(func() { buckets, conns = len(h.subs), len(h.conns) })
	if buckets != 0 || conns != 0 {
		t.Fatalf("after unregister: %d routing buckets and %d connections, want 0 and 0", buckets, conns)
	}
	if h.subCount != 0 {
		t.Fatalf("subscriptions_active would report %d after every client left", h.subCount)
	}
}

// -----------------------------------------------------------------------------
// Resync
// -----------------------------------------------------------------------------

func TestClientRequestedResyncResendsEveryHeldChannel(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	ctx := context.Background()
	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	c := joinHub(t, h, newFakeSocket(), nil)
	h.subscribe(ctx, c, []string{"market:" + sampleMarketID, "league:" + sampleLeagueSlug}, "")
	_ = queued(t, c)

	// An empty channel list means "everything I hold", which is what a client
	// sends after seeing a sequence gap: it knows something was lost, not which
	// channel lost it.
	h.resync(ctx, c, nil, "req-r", ResyncClientRequested)

	frames := queued(t, c)
	if len(frames) != 2 {
		t.Fatalf("got %v, want one snapshot per held channel", kinds(frames))
	}
	for _, f := range frames {
		if f.Type != KindSnapshot {
			t.Fatalf("got %v, want two snapshots", kinds(frames))
		}
		if f.ID != "req-r" {
			t.Fatalf("snapshot echoes id %q, want req-r", f.ID)
		}
		if len(f.Markets) != 1 {
			t.Fatalf("snapshot for %s carries %d markets, want 1", f.Channel, len(f.Markets))
		}
	}
	if n := counter(t, reg, "sharpline_ws_resyncs_total", "reason", string(ResyncClientRequested)); n != 1 {
		t.Fatalf("resyncs_total{client_requested} = %v, want 1", n)
	}
}

func TestResyncOnAnEmptySubscriptionSetIsNotCounted(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)
	c := joinHub(t, h, newFakeSocket(), nil)

	h.resync(context.Background(), c, nil, "", ResyncClientRequested)

	if n := counter(t, reg, "sharpline_ws_resyncs_total", "reason", string(ResyncClientRequested)); n != 0 {
		t.Fatalf("resyncs_total{client_requested} = %v; nothing was resynchronised, so "+
			"counting it would put noise into WebSocketResyncStorm's numerator", n)
	}
}

// -----------------------------------------------------------------------------
// Readiness (D1)
// -----------------------------------------------------------------------------

func TestReadinessRequiresACompleteSlate(t *testing.T) {
	t.Parallel()

	source := newStubSource(versioned(t, 1), versioned(t, 2))
	h, _ := newTestHub(t, func(o *HubOptions) { o.Source = source })

	if err := h.Check(context.Background()); err == nil {
		t.Fatal("a hub that has not started reported ready")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	<-source.replay
	waitFor(t, func() bool { return h.Check(context.Background()) == nil })

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := h.Check(context.Background()); err == nil {
		t.Fatal("a hub whose follower has stopped reported ready; a replica that is not " +
			"consuming must not be routed traffic")
	}
}

func TestReadinessIsRefusedWhileTheReplayIsInFlight(t *testing.T) {
	t.Parallel()

	source := &blockingSource{released: make(chan struct{})}
	h, _ := newTestHub(t, func(o *HubOptions) { o.Source = source })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()

	waitFor(t, func() bool { return h.running.Load() })
	if err := h.Check(context.Background()); err == nil {
		t.Fatal("a replica reported ready with a partial slate; every snapshot it served " +
			"would be silently missing markets")
	}

	close(source.released)
	waitFor(t, func() bool { return h.Check(context.Background()) == nil })
	cancel()
	<-done
}

// blockingSource holds the replay open until it is released, so the
// not-yet-caught-up window can be observed rather than raced past.
type blockingSource struct {
	released chan struct{}
	caught   atomic.Bool
}

func (s *blockingSource) Run(ctx context.Context, _ kafka.Handler) error {
	select {
	case <-s.released:
	case <-ctx.Done():
		return nil
	}
	s.caught.Store(true)
	<-ctx.Done()
	return nil
}

func (s *blockingSource) HasCaughtUp() bool { return s.caught.Load() }

func TestRunWithoutASourceRefusesRatherThanPretendingToConsume(t *testing.T) {
	t.Parallel()

	h, _ := newTestHub(t, nil)
	if err := h.Run(context.Background()); err == nil {
		t.Fatal("Run with no bus source returned nil")
	}
}

func TestAPoisonRecordIsSkippedRatherThanStoppingTheFollower(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, nil)

	bad := versioned(t, 1)
	bad.Envelope.Data = json.RawMessage(`{"schema_version":0}`)

	if err := h.HandleMessage(context.Background(), bad); err != nil {
		t.Fatalf("a rejected record returned %v; returning an error to the follower would "+
			"wedge the fanout for every client for ever", err)
	}
	if n := counter(t, reg, "sharpline_ws_bus_records_total", "result", "rejected"); n != 1 {
		t.Fatalf("bus_records_total{rejected} = %v, want 1 — the failure must be loud on "+
			"the dashboard instead of silent", n)
	}
}

// -----------------------------------------------------------------------------
// Presence (D6)
// -----------------------------------------------------------------------------

func TestASessionResumesItsChannelsOnAnotherReplica(t *testing.T) {
	t.Parallel()

	presence := newCountingPresence()
	presence.channels = []string{"market:" + sampleMarketID, "league:" + sampleLeagueSlug}

	h, _ := newTestHub(t, func(o *HubOptions) { o.Presence = presence })
	ctx := context.Background()
	if err := h.HandleMessage(ctx, versioned(t, 1)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}

	c := newConn(connOptions{
		ID:        "resumed",
		SessionID: "sess-1",
		Resumable: true,
		Identity:  Identity{UserID: "u-1", SessionID: "sess-1"},
		Socket:    newFakeSocket(),
		Hub:       h,
		Options:   h.Options(),
		Logger:    testLogger(),
		Now:       h.now,
	})
	if err := h.Register(ctx, c); err != nil {
		t.Fatalf("Register: %v", err)
	}

	frames := queued(t, c)
	if len(frames) != 3 {
		t.Fatalf("got %v, want hello and one snapshot per restored channel", kinds(frames))
	}
	hello := frames[0]
	if hello.Type != KindHello || !hello.Resumed {
		t.Fatalf("hello = {%s resumed:%v}, want a resumed hello — this field is what makes "+
			"affinity-free routing observable from the client", hello.Type, hello.Resumed)
	}
	if len(hello.Channels) != 2 {
		t.Fatalf("hello restored %v, want both channels", hello.Channels)
	}
	for _, f := range frames[1:] {
		if f.Type != KindSnapshot {
			t.Fatalf("got %v, want snapshots after the hello", kinds(frames))
		}
	}
	if presence.count("connected") != 1 {
		t.Fatal("the connection was not recorded in fleet presence")
	}
}

func TestAnUnreachablePresenceStoreStillServesTheConnection(t *testing.T) {
	t.Parallel()

	presence := newCountingPresence()
	presence.fail = fmt.Errorf("redis is unreachable")

	h, reg := newTestHub(t, func(o *HubOptions) { o.Presence = presence })
	ctx := context.Background()

	c := newConn(connOptions{
		ID:        "degraded",
		SessionID: "sess-2",
		Resumable: true,
		Socket:    newFakeSocket(),
		Hub:       h,
		Options:   h.Options(),
		Logger:    testLogger(),
		Now:       h.now,
	})
	if err := h.Register(ctx, c); err != nil {
		t.Fatalf("Register refused a connection because Redis was down: %v", err)
	}
	h.subscribe(ctx, c, []string{"market:" + sampleMarketID}, "")

	frames := queued(t, c)
	if len(frames) != 3 || frames[0].Type != KindHello {
		t.Fatalf("got %v, want hello, ack, snapshot — the socket must be unaffected", kinds(frames))
	}
	if frames[0].Resumed {
		t.Fatal("hello claims a resumed session that could not be read")
	}

	// Every failure is counted, per operation, so the degradation is visible
	// rather than merely tolerated.
	total := 0.0
	for _, op := range PresenceOps() {
		total += counter(t, reg, "sharpline_ws_presence_errors_total", "op", string(op))
	}
	if total == 0 {
		t.Fatal("presence failures were swallowed; D6 requires them counted")
	}
}

// -----------------------------------------------------------------------------
// Shutdown (D11)
// -----------------------------------------------------------------------------

func TestShutdownTellsEveryClientBeforeClosingIt(t *testing.T) {
	t.Parallel()

	h, reg := newTestHub(t, func(o *HubOptions) { o.Options.ShutdownDrain = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	socks := make([]*fakeSocket, 3)
	dones := make([]chan struct{}, 3)
	for i := range socks {
		socks[i] = newFakeSocket()
		c := joinHub(t, h, socks[i], nil)
		done := make(chan struct{})
		dones[i] = done
		go func() { defer close(done); c.serve(ctx) }()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	for i, done := range dones {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("connection %d was still serving after Shutdown returned", i)
		}
	}

	for i, sock := range socks {
		var sawGoingAway bool
		for _, raw := range sock.frames() {
			f := decodeTestFrame(t, raw)
			if f.Type == KindError && f.Code == CodeGoingAway {
				sawGoingAway = true
			}
		}
		if !sawGoingAway {
			t.Fatalf("connection %d was closed without being told why; a client cannot tell "+
				"a deploy from a dead socket", i)
		}
		if code, _ := sock.closeSpec(); code != websocket.StatusGoingAway {
			t.Fatalf("connection %d closed with status %d, want %d", i, code, websocket.StatusGoingAway)
		}
	}

	if n := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(DropShutdown)); n != 3 {
		t.Fatalf("clients_dropped_total{shutdown} = %v, want 3 — a deploy must not read as "+
			"a fanout regression", n)
	}

	// Idempotent, and it refuses new connections afterwards.
	if err := h.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("a second Shutdown returned %v", err)
	}
	late := newConn(connOptions{
		ID: "late", Socket: newFakeSocket(), Hub: h,
		Options: h.Options(), Logger: testLogger(), Now: h.now,
	})
	if err := h.Register(context.Background(), late); err == nil {
		t.Fatal("a connection was admitted after Shutdown")
	}
}
