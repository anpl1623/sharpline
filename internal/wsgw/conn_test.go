// The connection: sequence assignment, the overflow path, the heartbeat reaper
// and the goroutine budget.
//
// Everything here runs over an in-memory socket rather than a real one. That is
// the reason [wsConn] exists as an interface: the two properties this file is
// really about — that a discarded buffer leaves a hole a client can SEE, and
// that no goroutine outlives a connection — are both about a client that has
// STOPPED READING, and stalling a real TCP peer means filling a kernel buffer
// whose size nobody controls. A test of D4 that depends on the size of a socket
// buffer is a test that passes on a laptop and flakes in CI.
package wsgw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
)

// -----------------------------------------------------------------------------
// Test doubles
// -----------------------------------------------------------------------------

// fakeSocket is an in-memory [wsConn].
//
// It is a stand-in for the TRANSPORT and for nothing else: every byte it
// records was rendered by the real protocol code, and every frame it delivers
// is decoded by the real decoder. What it fakes is the ability to stop reading
// on command, which is the one thing a real socket cannot be asked to do
// deterministically.
type fakeSocket struct {
	mu        sync.Mutex
	written   [][]byte
	readLimit int64
	closeCode websocket.StatusCode
	closeText string
	gate      chan struct{}

	reads     chan fakeRead
	closedCh  chan struct{}
	closeOnce sync.Once

	pings      int
	pingStalls bool
}

type fakeRead struct {
	typ  websocket.MessageType
	data []byte
	err  error
}

func newFakeSocket() *fakeSocket {
	return &fakeSocket{
		reads:    make(chan fakeRead, 16),
		closedCh: make(chan struct{}),
	}
}

// stall makes every subsequent Write block until resume is called. It is how a
// client that has stopped reading is expressed.
func (f *fakeSocket) stall() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gate == nil {
		f.gate = make(chan struct{})
	}
}

func (f *fakeSocket) resume() {
	f.mu.Lock()
	gate := f.gate
	f.gate = nil
	f.mu.Unlock()
	if gate != nil {
		close(gate)
	}
}

func (f *fakeSocket) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case r := <-f.reads:
		if r.err != nil {
			return 0, nil, r.err
		}
		return r.typ, r.data, nil
	case <-f.closedCh:
		return 0, nil, io.EOF
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (f *fakeSocket) Write(ctx context.Context, _ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-f.closedCh:
			return errors.New("fakeSocket: closed while writing")
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	select {
	case <-f.closedCh:
		return errors.New("fakeSocket: write on a closed connection")
	default:
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.written = append(f.written, append([]byte(nil), p...))
	return nil
}

func (f *fakeSocket) Ping(ctx context.Context) error {
	f.mu.Lock()
	f.pings++
	stalls := f.pingStalls
	f.mu.Unlock()
	if !stalls {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeSocket) pingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *fakeSocket) Close(code websocket.StatusCode, reason string) error {
	f.mu.Lock()
	if f.closeCode == 0 {
		f.closeCode, f.closeText = code, reason
	}
	f.mu.Unlock()
	f.closeOnce.Do(func() { close(f.closedCh) })
	return nil
}

func (f *fakeSocket) CloseNow() error { return f.Close(websocket.StatusAbnormalClosure, "") }

func (f *fakeSocket) SetReadLimit(n int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readLimit = n
}

func (f *fakeSocket) frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.written...)
}

func (f *fakeSocket) closeSpec() (websocket.StatusCode, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCode, f.closeText
}

func (f *fakeSocket) limit() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readLimit
}

// client pushes one text frame at the connection, as a browser would.
func (f *fakeSocket) client(raw string) {
	f.reads <- fakeRead{typ: websocket.MessageText, data: []byte(raw)}
}

// stubHub records what a connection asked the hub for.
type stubHub struct {
	mu          sync.Mutex
	subscribes  [][]string
	unsubs      [][]string
	resyncs     [][]string
	heartbeats  int
	unregisters int
}

func (s *stubHub) subscribe(_ context.Context, _ *conn, channels []string, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribes = append(s.subscribes, channels)
}

func (s *stubHub) unsubscribe(_ context.Context, _ *conn, channels []string, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unsubs = append(s.unsubs, channels)
}

func (s *stubHub) resync(_ context.Context, _ *conn, channels []string, _ string, _ ResyncReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncs = append(s.resyncs, channels)
}

func (s *stubHub) heartbeat(context.Context, *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats++
}

func (s *stubHub) unregister(context.Context, *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unregisters++
}

func (s *stubHub) counts() (subs, unsubs, resyncs, beats, unregs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subscribes), len(s.unsubs), len(s.resyncs), s.heartbeats, s.unregisters
}

// -----------------------------------------------------------------------------
// Shared helpers
// -----------------------------------------------------------------------------

// testLogger discards output. These tests assert on frames and metrics, never
// on log lines.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testMetrics builds a collector set on a fresh registry, so one test's
// counters can never be read by another.
func testMetrics(t *testing.T) (*Metrics, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	return m, reg
}

// counter reads one counter sample by label value, or 0 when the series has no
// such child yet.
func counter(t *testing.T, reg *prometheus.Registry, name, label, value string) float64 {
	t.Helper()
	families := gather(t, reg)
	f, ok := families[name]
	if !ok {
		return 0
	}
	for _, m := range f.GetMetric() {
		if labelSet(m)[label] == value {
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// frameSeqs decodes the sequence numbers off a run of frames.
func frameSeqs(t *testing.T, raw [][]byte) []uint64 {
	t.Helper()
	out := make([]uint64, 0, len(raw))
	for _, b := range raw {
		var f struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("frame %q is not JSON: %v", b, err)
		}
		out = append(out, f.Seq)
	}
	return out
}

// newTestConn builds a connection over a fake socket. It starts nothing.
func newTestConn(t *testing.T, sock wsConn, hub connHub, mutate func(*Options)) *conn {
	t.Helper()
	m, _ := testMetrics(t)
	opts := Options{Logger: testLogger(), Metrics: m}
	if mutate != nil {
		mutate(&opts)
	}
	opts = opts.Normalise()
	if err := opts.Validate(); err != nil {
		t.Fatalf("test options do not validate: %v", err)
	}
	return newConn(connOptions{
		ID:      "conn-test",
		Socket:  sock,
		Hub:     hub,
		Options: opts,
		Logger:  testLogger(),
		Now:     time.Now,
		Remote:  "203.0.113.7",
	})
}

// -----------------------------------------------------------------------------
// Sequence assignment (D3)
// -----------------------------------------------------------------------------

func TestSequenceStartsAtOneAndAdvancesForEveryFrame(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	c := newTestConn(t, sock, &stubHub{}, nil)

	// Every kind, because D3 says the counter advances for all of them and the
	// tempting bug is to skip the ones that are not deltas.
	bodies := []FrameBody{
		NewHello("conn-test", "sess", 20*time.Second, false, false, nil),
		NewAck([]Channel{mustChannel(t, "market:mkt-1")}, nil),
		NewSnapshot(mustChannel(t, "market:mkt-1"), nil),
		NewDelta(mustChannel(t, "market:mkt-1"), json.RawMessage(`{"a":1}`)),
		NewResync(ResyncClientRequested, 0, 0, 0),
		NewError(CodeInternal, "x"),
		NewPong(),
	}
	for _, b := range bodies {
		if !c.sendFrame(b, "") {
			t.Fatalf("sendFrame(%s) refused", b.Kind())
		}
	}

	var queued [][]byte
	for len(c.send) > 0 {
		queued = append(queued, (<-c.send).payload)
	}
	got := frameSeqs(t, queued)
	if len(got) != len(bodies) {
		t.Fatalf("queued %d frames, want %d", len(got), len(bodies))
	}
	for i, seq := range got {
		if seq != uint64(i+1) {
			t.Fatalf("frame %d carries seq %d, want %d", i, seq, i+1)
		}
	}
}

func TestGapRecordsOnlyWhatWasDiscarded(t *testing.T) {
	t.Parallel()

	var g gap
	if !g.empty() {
		t.Fatal("a fresh gap is not empty")
	}
	g.extend(0) // a zero can never name a frame
	if !g.empty() {
		t.Fatal("a zero sequence number was recorded as a discarded frame")
	}
	for _, seq := range []uint64{7, 8, 9} {
		g.extend(seq)
	}
	if g.from != 7 || g.to != 9 || g.dropped != 3 {
		t.Fatalf("gap = {from:%d to:%d dropped:%d}, want {7 9 3}", g.from, g.to, g.dropped)
	}
}

// -----------------------------------------------------------------------------
// Backpressure (D4)
// -----------------------------------------------------------------------------

func TestOverflowDiscardsTheBufferAndQueuesAResync(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	m, reg := testMetrics(t)
	c := newTestConn(t, sock, &stubHub{}, func(o *Options) {
		o.SendQueueCapacity = 4
		o.Metrics = m
	})

	// No writer is running, so the queue fills at exactly its capacity and the
	// fifth frame is the one that overflows. Determinism matters here more than
	// realism: the property under test is arithmetic, not timing.
	for i := 0; i < 4; i++ {
		if !c.sendFrame(NewPong(), "") {
			t.Fatalf("frame %d was refused before the queue was full", i+1)
		}
	}
	if c.sendFrame(NewPong(), "") {
		t.Fatal("the fifth frame was accepted into a queue of four")
	}

	if got := len(c.send); got != 1 {
		t.Fatalf("queue holds %d frames after the discard, want exactly the resync", got)
	}

	out := <-c.send
	if out.kind != KindResync {
		t.Fatalf("the surviving frame is %s, want %s", out.kind, KindResync)
	}

	var frame struct {
		Seq     uint64       `json:"seq"`
		Type    MessageKind  `json:"type"`
		Reason  ResyncReason `json:"reason"`
		Dropped int          `json:"dropped"`
		FromSeq uint64       `json:"from_seq"`
		ToSeq   uint64       `json:"to_seq"`
	}
	if err := json.Unmarshal(out.payload, &frame); err != nil {
		t.Fatalf("resync frame is not JSON: %v", err)
	}

	// Frames 1..4 were queued and discarded; frame 5 was assigned a number and
	// never queued. Both are lost, so the range is [1,5] and the count is 5.
	// The resync itself is 6, which is what makes the hole visible: a client
	// that saw nothing then receives seq 6 and is told why.
	if frame.Type != KindResync || frame.Reason != ResyncSlowConsumer {
		t.Fatalf("frame = {%s %s}, want {resync slow_consumer}", frame.Type, frame.Reason)
	}
	if frame.FromSeq != 1 || frame.ToSeq != 5 || frame.Dropped != 5 {
		t.Fatalf("gap = [%d,%d] dropped=%d, want [1,5] dropped=5",
			frame.FromSeq, frame.ToSeq, frame.Dropped)
	}
	if frame.Seq != 6 {
		t.Fatalf("resync carries seq %d, want 6 — it must continue from the highest assigned", frame.Seq)
	}

	if got := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(DropSlowConsumer)); got != 1 {
		t.Fatalf("clients_dropped_total{slow_consumer} = %v, want 1", got)
	}
	if got := counter(t, reg, "sharpline_ws_resyncs_total", "reason", string(ResyncSlowConsumer)); got != 1 {
		t.Fatalf("resyncs_total{slow_consumer} = %v, want 1", got)
	}
	if got := counter(t, reg, "sharpline_ws_messages_sent_total", "kind", string(KindResync)); got != 1 {
		t.Fatalf("messages_sent_total{resync} = %v, want 1", got)
	}
}

func TestOverflowLeavesAGapOnTheWire(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	c := newTestConn(t, sock, &stubHub{}, func(o *Options) { o.SendQueueCapacity = 2 })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	// The sequence below is deliberately choreographed rather than a flood.
	// The claim being tested is exact — "the client sees 1, 2, then 6" — and a
	// flood would only prove that something was lost, which is the weaker half.

	// 1. One frame goes out normally, so the client has a baseline.
	c.sendFrame(NewPong(), "")
	waitFor(t, func() bool { return len(sock.frames()) == 1 })

	// 2. The client stops reading. The next frame is popped by the writer and
	//    parks inside the socket write, which is what a real stalled peer does.
	sock.stall()
	c.sendFrame(NewPong(), "")
	waitFor(t, func() bool { return len(c.send) == 0 })

	// 3. Two frames fill the queue and the third overflows it.
	for i := 0; i < 3; i++ {
		c.sendFrame(NewPong(), "")
	}

	// 4. The client starts reading again.
	sock.resume()
	waitFor(t, func() bool { return len(sock.frames()) == 3 })

	c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-served

	frames := sock.frames()
	seqs := frameSeqs(t, frames)
	want := []uint64{1, 2, 6}
	if len(seqs) != len(want) {
		t.Fatalf("the client received %v, want %v", seqs, want)
	}
	for i := range want {
		if seqs[i] != want[i] {
			t.Fatalf("the client received %v, want %v — frames 3, 4 and 5 were "+
				"assigned numbers and discarded, so the hole must be visible", seqs, want)
		}
	}

	var last struct {
		Type    MessageKind  `json:"type"`
		Reason  ResyncReason `json:"reason"`
		Dropped int          `json:"dropped"`
		FromSeq uint64       `json:"from_seq"`
		ToSeq   uint64       `json:"to_seq"`
	}
	if err := json.Unmarshal(frames[2], &last); err != nil {
		t.Fatalf("last frame is not JSON: %v", err)
	}
	if last.Type != KindResync || last.Reason != ResyncSlowConsumer {
		t.Fatalf("last frame is {%s %s}, want {resync slow_consumer}", last.Type, last.Reason)
	}
	if last.FromSeq != 3 || last.ToSeq != 5 || last.Dropped != 3 {
		t.Fatalf("resync reports [%d,%d] dropped=%d, want [3,5] dropped=3 — the range "+
			"must name exactly the frames the client did not get",
			last.FromSeq, last.ToSeq, last.Dropped)
	}
}

// -----------------------------------------------------------------------------
// Heartbeat and idle reaping (D10)
// -----------------------------------------------------------------------------

func TestHeartbeatRefreshesTheSessionLease(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	hub := &stubHub{}
	c := newTestConn(t, sock, hub, func(o *Options) {
		o.PingInterval = 15 * time.Millisecond
		o.PongTimeout = 5 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	waitFor(t, func() bool {
		_, _, _, beats, _ := hub.counts()
		return beats >= 2
	})
	if got := sock.pingCount(); got < 2 {
		t.Fatalf("the lease was refreshed %d times after only %d control-frame round trips; "+
			"the refresh must follow evidence of liveness, not a timer", 2, got)
	}

	c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-served
}

func TestIdleConnectionIsReapedWhenNoPongArrives(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	sock.pingStalls = true

	m, reg := testMetrics(t)
	c := newTestConn(t, sock, &stubHub{}, func(o *Options) {
		o.PingInterval = 15 * time.Millisecond
		o.PongTimeout = 5 * time.Millisecond
		o.Metrics = m
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a connection that never answered a ping was not reaped")
	}

	c.mu.Lock()
	reason := c.closeReason
	c.mu.Unlock()
	if reason != DropIdleTimeout {
		t.Fatalf("close reason is %q, want %q", reason, DropIdleTimeout)
	}
	if got := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(DropIdleTimeout)); got != 1 {
		t.Fatalf("clients_dropped_total{idle_timeout} = %v, want 1", got)
	}
	if code, _ := sock.closeSpec(); code != websocket.StatusPolicyViolation {
		t.Fatalf("close status is %d, want %d", code, websocket.StatusPolicyViolation)
	}
}

// -----------------------------------------------------------------------------
// Reading, dispatch and protocol violations
// -----------------------------------------------------------------------------

func TestReadLimitSitsJustAboveTheFrameCeiling(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	c := newTestConn(t, sock, &stubHub{}, func(o *Options) { o.MaxFrameBytes = 4096 })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	waitFor(t, func() bool { return sock.limit() != 0 })
	if got, want := sock.limit(), int64(4096+readLimitHeadroom); got != want {
		t.Fatalf("read limit is %d, want %d — the library must not refuse a frame "+
			"before DecodeClient can answer it with a coded error", got, want)
	}

	c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-served
}

func TestClientFramesAreDispatchedToTheHub(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	hub := &stubHub{}
	c := newTestConn(t, sock, hub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	sock.client(`{"type":"subscribe","id":"a","channels":["market:mkt-1"]}`)
	sock.client(`{"type":"unsubscribe","id":"b","channels":["market:mkt-1"]}`)
	sock.client(`{"type":"resync","id":"c"}`)
	sock.client(`{"type":"ping","id":"d"}`)

	waitFor(t, func() bool {
		subs, unsubs, resyncs, _, _ := hub.counts()
		return subs == 1 && unsubs == 1 && resyncs == 1
	})
	waitFor(t, func() bool {
		for _, raw := range sock.frames() {
			if strings.Contains(string(raw), `"type":"pong"`) {
				return true
			}
		}
		return false
	})

	c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-served
}

func TestProtocolViolationIsAnsweredBeforeTheConnectionCloses(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		send string
		code ErrorCode
		drop DropReason
	}{
		{"undecodable", `{`, CodeMalformedFrame, DropProtocolError},
		{"unknown type", `{"type":"shout"}`, CodeUnknownType, DropProtocolError},
		{"unknown field", `{"type":"subscribe","channel":"market:mkt-1"}`, CodeMalformedFrame, DropProtocolError},
		{"empty subscribe", `{"type":"subscribe","channels":[]}`, CodeMalformedFrame, DropProtocolError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sock := newFakeSocket()
			c := newTestConn(t, sock, &stubHub{}, nil)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			served := make(chan struct{})
			go func() { defer close(served); c.serve(ctx) }()

			sock.client(tc.send)

			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("the connection was not closed after a protocol violation")
			}

			// The error frame must have reached the socket. That is what the
			// wind-down order in conn.go exists for: closing first would
			// truncate the one frame that explains the closure.
			var sawError bool
			for _, raw := range sock.frames() {
				var f struct {
					Type    MessageKind `json:"type"`
					Code    ErrorCode   `json:"code"`
					Message string      `json:"message"`
				}
				if err := json.Unmarshal(raw, &f); err != nil {
					t.Fatalf("frame is not JSON: %v", err)
				}
				if f.Type != KindError {
					continue
				}
				sawError = true
				if f.Code != tc.code {
					t.Fatalf("error code is %q, want %q", f.Code, tc.code)
				}
				if strings.Contains(f.Message, tc.send) {
					t.Fatalf("the error message echoes the client's input: %q", f.Message)
				}
			}
			if !sawError {
				t.Fatal("no error frame reached the client before the close")
			}

			c.mu.Lock()
			reason := c.closeReason
			c.mu.Unlock()
			if reason != tc.drop {
				t.Fatalf("close reason is %q, want %q", reason, tc.drop)
			}
		})
	}
}

func TestBinaryFramesAreRefused(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	c := newTestConn(t, sock, &stubHub{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	sock.reads <- fakeRead{typ: websocket.MessageBinary, data: []byte{0x00, 0x01}}

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("a binary frame did not end the connection")
	}
	c.mu.Lock()
	reason := c.closeReason
	c.mu.Unlock()
	if reason != DropProtocolError {
		t.Fatalf("close reason is %q, want %q", reason, DropProtocolError)
	}
}

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

func TestCloseIsIdempotentAndKeepsTheFirstReason(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	m, reg := testMetrics(t)
	c := newTestConn(t, sock, &stubHub{}, func(o *Options) { o.Metrics = m })

	c.requestClose(DropIdleTimeout, websocket.StatusPolicyViolation, "first")
	c.requestClose(DropWriteError, websocket.StatusInternalError, "second")
	c.requestClose(DropShutdown, websocket.StatusGoingAway, "third")

	c.mu.Lock()
	reason, text := c.closeReason, c.closeText
	c.mu.Unlock()
	if reason != DropIdleTimeout || text != "first" {
		t.Fatalf("close recorded {%q %q}, want the FIRST caller's {idle_timeout first} — "+
			"a later symptom must not overwrite the cause", reason, text)
	}
	if got := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(DropIdleTimeout)); got != 1 {
		t.Fatalf("clients_dropped_total{idle_timeout} = %v, want exactly 1", got)
	}
	for _, other := range []DropReason{DropWriteError, DropShutdown} {
		if got := counter(t, reg, "sharpline_ws_clients_dropped_total", "reason", string(other)); got != 0 {
			t.Fatalf("clients_dropped_total{%s} = %v, want 0", other, got)
		}
	}
}

func TestCloseReasonIsBoundedToAValidControlFrame(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", 500)
	if got := len(truncateCloseReason(long)); got > 123 {
		t.Fatalf("close reason is %d bytes; RFC 6455 caps a control frame payload at 125, "+
			"and an over-long reason invalidates the close frame itself", got)
	}
	if truncateCloseReason("short") != "short" {
		t.Fatal("a short reason was truncated")
	}
}

func TestServingLeavesNoGoroutineBehind(t *testing.T) {
	t.Parallel()

	baseline := goroutineBaseline()

	for i := 0; i < 20; i++ {
		sock := newFakeSocket()
		c := newTestConn(t, sock, &stubHub{}, nil)

		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan struct{})
		go func() { defer close(served); c.serve(ctx) }()

		c.sendFrame(NewPong(), "")
		c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
		<-served
		cancel()
	}

	// A leak here is a leak PER CLIENT, which is the worst shape it can have on
	// a service whose stated target is ten thousand of them.
	waitFor(t, func() bool { return runtime.NumGoroutine() <= baseline+2 })
}

func TestServeUnregistersExactlyOnce(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	hub := &stubHub{}
	c := newTestConn(t, sock, hub, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan struct{})
	go func() { defer close(served); c.serve(ctx) }()

	c.requestClose(DropShutdown, websocket.StatusGoingAway, "test over")
	<-served

	if _, _, _, _, unregs := hub.counts(); unregs != 1 {
		t.Fatalf("unregister ran %d times, want exactly 1", unregs)
	}
}

func TestEnqueueAfterCloseIsRefusedRatherThanQueued(t *testing.T) {
	t.Parallel()

	sock := newFakeSocket()
	c := newTestConn(t, sock, &stubHub{}, nil)
	c.requestClose(DropShutdown, websocket.StatusGoingAway, "gone")

	if c.sendFrame(NewPong(), "") {
		t.Fatal("a frame was queued onto a closed connection")
	}
	if len(c.send) != 0 {
		t.Fatalf("the queue holds %d frames after the close", len(c.send))
	}
}

// -----------------------------------------------------------------------------
// Small helpers
// -----------------------------------------------------------------------------

// waitFor polls a condition. Every use here is waiting on another goroutine to
// reach a state, which has no channel to select on without reaching into the
// production types purely for the test's benefit.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not reached within the deadline")
}

// goroutineBaseline settles the scheduler and reports the count to compare
// against.
func goroutineBaseline() int {
	for i := 0; i < 10; i++ {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	return runtime.NumGoroutine()
}
