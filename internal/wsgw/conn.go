// One client connection: the bounded send queue, the three goroutines that
// serve it, and the two ways it can die.
//
// # The shape, and why it is three goroutines and not one
//
//	readLoop   runs on the HTTP handler's own goroutine. It must, because the
//	           handler may not return while the hijacked connection is live.
//	writeLoop  drains the bounded queue to the socket. It exists so that the
//	           hub never performs a syscall while holding the state lock —
//	           which is the whole of D4.
//	pingLoop   sends an RFC 6455 control ping every PingInterval and reaps a
//	           connection that does not answer (D10).
//
// A single-goroutine design is possible and is worse: the read is blocking with
// no useful deadline (a client that says nothing for an hour is healthy), so
// either the writes wait behind it or the reads are polled. Three loops with
// one owner is the honest structure.
//
// # No goroutine may outlive the connection
//
// This is stated as a rule rather than left to review because a leak here is a
// leak PER CLIENT, which is the worst shape a leak can have: it scales with
// exactly the number this service is built to make large. [conn.serve] starts
// every goroutine it owns, waits for all of them, and only then returns, and
// conn_test.go asserts the goroutine count returns to its baseline.
//
// The wind-down order is load-bearing and is not the obvious one:
//
//  1. something calls [conn.requestClose], which records WHY and closes `quit`;
//  2. writeLoop sees `quit`, DRAINS what is already queued (bounded), and
//     returns — so the `error` frame a protocol violation just enqueued, and
//     the going-away frame D11 enqueues, actually reach the client;
//  3. only then is the WebSocket close frame written, which is what unblocks
//     readLoop;
//  4. serve waits for every goroutine and releases the socket.
//
// Writing the close frame before the drain would truncate the last thing the
// client needed to read, which is the one frame that explains the closure.
//
// # The hub is never blocked by a client
//
// Every method the hub calls into — [conn.enqueue], [conn.send],
// [conn.requestClose] — is non-blocking. [conn.enqueue] runs under the STORE's
// write lock (state.go's publish callback), so anything that blocked in it
// would stall the fanout for every other connection on the replica. The lock
// order is therefore fixed and one-way: store lock, then connection lock, never
// the reverse.
package wsgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// -----------------------------------------------------------------------------
// Seams
// -----------------------------------------------------------------------------

// wsConn is the part of *websocket.Conn a connection uses.
//
// It is declared here, by the consumer (CLAUDE.md §12), for a reason beyond
// convention: it is what lets conn_test.go drive the overflow path and the
// heartbeat reaper over an in-memory socket whose reader can be STALLED on
// demand. Stalling a real TCP peer means filling a kernel buffer, which is
// timing-dependent and would make the one test that proves D4 the flakiest test
// in the repository.
//
// SetReadLimit is included even though it is called once, because a fake that
// silently ignored it would let a test pass while the real connection had no
// bound at all.
type wsConn interface {
	Read(ctx context.Context) (websocket.MessageType, []byte, error)
	Write(ctx context.Context, typ websocket.MessageType, p []byte) error
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow() error
	SetReadLimit(n int64)
}

// Compile-time proof that the shipped socket satisfies the declaration. It is
// here rather than at the call site because a mismatch should break THIS
// package's build, where the interface is declared — the same argument
// internal/pricing makes for its own seams.
var _ wsConn = (*websocket.Conn)(nil)

// connHub is the part of the hub one connection drives.
//
// Both halves of this package could reach into each other directly — they are
// one package — and the interface exists anyway, for two reasons. It states
// exactly what a connection is allowed to ask of the hub, which is a short
// list; and it lets conn_test.go exercise the read loop, the queue and the
// heartbeat with no bus, no slate and no Prometheus registry.
//
// Every method must be non-blocking or bounded. A connection's read loop
// calling into the hub is a client's traffic reaching the shared state lock, so
// anything slow here is a client's ability to stall the fanout.
type connHub interface {
	// subscribe adds channels, answers with an ack and a snapshot per accepted
	// channel (D2). Raw strings, because parsing them is where the bounded
	// rejection reasons come from.
	subscribe(ctx context.Context, c *conn, channels []string, requestID string)

	// unsubscribe removes channels and answers with an ack.
	unsubscribe(ctx context.Context, c *conn, channels []string, requestID string)

	// resync re-sends a snapshot for the named channels; an empty list means
	// every channel this connection holds.
	resync(ctx context.Context, c *conn, channels []string, requestID string, reason ResyncReason)

	// heartbeat is called after each successful control-frame round trip. It is
	// where the durable subscription set's TTL is refreshed (D6).
	heartbeat(ctx context.Context, c *conn)

	// unregister removes the connection from the routing table. It is called
	// exactly once, by serve, after every loop this connection owns has stopped.
	unregister(ctx context.Context, c *conn)
}

// -----------------------------------------------------------------------------
// The queue
// -----------------------------------------------------------------------------

// outbound is one frame waiting for the socket.
//
// It carries its own sequence number and enqueue instant rather than deriving
// them later: the sequence number is what the gap accounting reports when the
// buffer is discarded, and the instant is the subtrahend of
// sharpline_ws_write_delay_seconds — the series that measures the difference
// between where the staleness SLO is observed (the queue hand-off) and where
// the bytes actually leave (the syscall).
type outbound struct {
	seq      uint64
	kind     MessageKind
	payload  []byte
	enqueued time.Time
}

// -----------------------------------------------------------------------------
// conn
// -----------------------------------------------------------------------------

// conn is one served WebSocket client.
//
// It is unexported because nothing outside this package holds one: the hub owns
// the registry, the upgrade handler builds them, and neither hands one out.
type conn struct {
	// id is the connection identifier announced in the `hello` frame. It is
	// what makes the per-connection sequence space unambiguous (D3).
	id string

	// sessionID is the durable session key the subscription set is stored under
	// (D6). Never empty on a served connection: an anonymous client is given a
	// fresh random one so that fleet presence covers every connection rather
	// than only the authenticated minority.
	sessionID string

	// resumable reports whether sessionID is a key the CLIENT can present
	// again, which today means it came from a verified token's session claim.
	//
	// A fresh random key for an anonymous connection is unique to that socket
	// and there is nothing to restore from it, so a restore would be a
	// guaranteed Redis miss on every anonymous connection — which is most of
	// them. The flag is what turns that into no round trip at all. See
	// server.go on why anonymous resume is not implemented rather than
	// implemented badly.
	resumable bool

	// identity is the verified identity, or the zero value for the anonymous
	// case — which is the normal case, because market data is public (D5).
	identity Identity

	// remote is the client address as best this service can determine it. It is
	// a LOG FIELD ONLY; nothing here makes a decision from it. See server.go.
	remote string

	ws  wsConn
	hub connHub
	m   *Metrics
	log *slog.Logger
	now func() time.Time

	// opts is the already-normalised gateway configuration.
	opts Options

	// send is the bounded queue (D4). Its capacity is the whole backpressure
	// policy: past it, the client's buffer is discarded rather than the hub
	// being made to wait.
	send chan outbound

	// quit is closed exactly once, by requestClose, and is how every loop
	// learns the connection is finished.
	quit chan struct{}

	// done is closed when serve returns and every goroutine this connection
	// owns has stopped. The hub's shutdown drain waits on it.
	done chan struct{}

	// mu guards seq, closed and the hand-off to send. It must be held across
	// ALL THREE — see sequence.go on why assigning the number outside the
	// critical section that queues the frame produces out-of-order sequences
	// that no client can recover from.
	mu     sync.Mutex
	seq    sequence
	closed bool

	closeOnce   sync.Once
	closeReason DropReason
	closeStatus websocket.StatusCode
	closeText   string

	// channels is this connection's subscription set.
	//
	// It is guarded by the STORE's write lock, not by mu, and that is
	// deliberate: it must change in the same critical section as the hub's
	// routing table, or the two can disagree about what this connection holds
	// and a delta is delivered to a channel the connection has left. Every
	// mutation runs inside Store.Attach or Store.Exclusive.
	channels map[Channel]struct{}
}

// connOptions are newConn's inputs. A struct rather than eight positional
// parameters, because six of them are strings and a transposition would compile.
type connOptions struct {
	ID        string
	SessionID string
	Resumable bool
	Identity  Identity
	Remote    string
	Socket    wsConn
	Hub       connHub
	Options   Options
	Logger    *slog.Logger
	Now       func() time.Time
}

// newConn builds a connection. It starts nothing; call [conn.serve].
//
// It returns no error: every value that could be wrong has already been
// validated by [Options.Validate] at startup, and a connection is built per
// client — a per-client error return would be a per-client branch that could
// only ever report a configuration mistake the process should not have started
// with.
func newConn(o connOptions) *conn {
	opts := o.Options.Normalise()
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	return &conn{
		id:        o.ID,
		sessionID: o.SessionID,
		resumable: o.Resumable,
		identity:  o.Identity,
		remote:    o.Remote,
		ws:        o.Socket,
		hub:       o.Hub,
		m:         opts.Metrics,
		log: log.With(
			slog.String("component", "wsgw.conn"),
			slog.String("connection_id", o.ID),
		),
		now:      now,
		opts:     opts,
		send:     make(chan outbound, opts.SendQueueCapacity),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		channels: make(map[Channel]struct{}),
	}
}

// LogValue implements slog.LogValuer so a whole connection can be logged
// without a call site deciding, each time, which of its fields are safe. The
// identity renders through [Identity.LogValue], which carries no credential.
func (c *conn) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("connection_id", c.id),
		slog.String("session_id", c.sessionID),
		slog.String("remote", c.remote),
		slog.Any("identity", c.identity),
	)
}

// -----------------------------------------------------------------------------
// Enqueue — D3 and D4
// -----------------------------------------------------------------------------

// send renders a single-recipient frame, stamps it and queues it.
//
// It is the path for hello, ack, snapshot, pong, error and resync — every frame
// with exactly one addressee. A delta going to many connections must NOT come
// through here: it renders once through [Render] and is queued with
// [conn.enqueue] per connection, which is the split protocol.go exists to make
// possible.
//
// It counts the frame in sharpline_ws_messages_sent_total. The fanout path
// counts in bulk instead ([Metrics.observeSentN]), so the two never
// double-count: [conn.enqueue] itself counts nothing except the resync it may
// manufacture, which no caller knows about.
func (c *conn) sendFrame(b FrameBody, requestID string) bool {
	body, err := Render(b, c.now(), requestID)
	if err != nil {
		// A frame this package could not render is a bug in this package, not
		// something the client did, and there is nothing useful to tell them.
		c.log.Error("dropping an unrenderable server frame",
			slog.String("kind", string(b.Kind())),
			slog.String("error", err.Error()))
		return false
	}
	kind := b.Kind()
	ok := c.enqueue(body, kind)
	c.m.observeSent(kind)
	return ok
}

// enqueue stamps the next sequence number onto a SHARED body and hands the
// resulting frame to this connection's bounded queue.
//
// It never blocks. The send is non-blocking with a default branch, because this
// runs under the store's write lock on the fanout path and a block here is a
// stall for every other client on the replica — CLAUDE.md §5 rules on it
// directly: "rather than letting one slow consumer apply backpressure to the
// entire hub".
//
// # On overflow (D4)
//
// The ENTIRE pending buffer is discarded, the discard is counted twice — once
// as sharpline_ws_clients_dropped_total{reason="slow_consumer"} and once as
// sharpline_ws_resyncs_total{reason="slow_consumer"} — and a `resync` frame is
// queued whose sequence number continues from the highest already assigned, so
// the client observes a real hole AND is told its size.
//
// The CONNECTION IS NOT CLOSED. That is worth stating because the metric is
// named clients_dropped_total and reads as though it were: CLAUDE.md §5 says to
// "drop the client's BUFFER and force a resync", so what is dropped is the
// buffer, and the client stays and resynchronises. The counter's name is fixed
// by the alert rules and cannot be changed; this comment is the reconciliation.
//
// The returned bool reports whether the frame itself survived. Callers that
// count messages_sent_total ignore it deliberately: the metric's help says it
// is counted at enqueue "so it agrees with the numbers a client actually sees
// rather than with the subset that survived a slow consumer", and a frame that
// consumed a sequence number is one of those numbers.
func (c *conn) enqueue(body Body, kind MessageKind) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}

	seq := c.seq.next()
	depth := len(c.send)
	select {
	case c.send <- outbound{seq: seq, kind: kind, payload: body.Frame(seq), enqueued: c.now()}:
		c.mu.Unlock()
		c.m.observeQueueDepth(depth)
		return true
	default:
	}

	// Overflow. Everything still queued is discarded, and so is this frame.
	g := c.discardLocked(seq)
	resync := c.resyncFrameLocked(g)
	c.mu.Unlock()

	c.m.observeQueueDepth(depth)
	c.m.observeDrop(DropSlowConsumer)
	c.m.observeResync(ResyncSlowConsumer)
	c.m.observeSent(KindResync)
	c.log.Warn("send queue overflowed; the client's pending buffer was discarded",
		slog.Int("capacity", c.opts.SendQueueCapacity),
		slog.Int("dropped", g.dropped),
		slog.Uint64("from_seq", g.from),
		slog.Uint64("to_seq", g.to),
		slog.Uint64("resync_seq", resync),
	)
	return false
}

// discardLocked empties the send queue and returns the range the client will
// not receive. The caller holds mu.
//
// pending is the sequence number of the frame that could not be queued; it is
// part of the gap, because it was assigned a number and will never be written.
//
// Only what is actually REMOVED is recorded. The writer goroutine may pop a
// frame while this loop runs, in which case that frame really is on its way to
// the client and is correctly absent from the range — which is why the range is
// accumulated rather than computed as (oldest, newest) from the counter.
func (c *conn) discardLocked(pending uint64) gap {
	var g gap
	for {
		select {
		case out := <-c.send:
			g.extend(out.seq)
		default:
			g.extend(pending)
			return g
		}
	}
}

// resyncFrameLocked queues the slow-consumer resync and returns its sequence
// number. The caller holds mu, and the queue has just been emptied.
func (c *conn) resyncFrameLocked(g gap) uint64 {
	if g.empty() {
		return 0
	}
	body, err := Render(NewResync(ResyncSlowConsumer, g.dropped, g.from, g.to), c.now(), "")
	if err != nil {
		c.log.Error("rendering the slow-consumer resync failed",
			slog.String("error", err.Error()))
		return 0
	}
	seq := c.seq.next()
	select {
	case c.send <- outbound{seq: seq, kind: KindResync, payload: body.Frame(seq), enqueued: c.now()}:
	default:
		// Unreachable: mu is held, the queue was emptied one statement ago, and
		// nothing but this method queues frames. The branch exists anyway
		// because "the hub never blocks on a client" has to be a property of
		// the code rather than of an argument about the code.
	}
	return seq
}

// -----------------------------------------------------------------------------
// Lifecycle
// -----------------------------------------------------------------------------

// closeSpec is the recorded reason for a closure.
func (c *conn) closeSpec() (websocket.StatusCode, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closeStatus == 0 {
		return websocket.StatusNormalClosure, ""
	}
	return c.closeStatus, c.closeText
}

// requestClose records why this connection is ending and wakes every loop.
//
// It is idempotent and safe from any goroutine, and the FIRST caller wins: a
// write error that follows an idle-timeout reap must not overwrite the reason,
// or sharpline_ws_clients_dropped_total attributes the closure to the symptom
// instead of the cause.
//
// It does not touch the socket. Writing the close frame here would race the
// drain of frames already queued — see the wind-down order in the file comment
// — so the socket is closed by serve, after the writer has finished.
func (c *conn) requestClose(reason DropReason, status websocket.StatusCode, text string) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.closeReason = reason
		c.closeStatus = status
		c.closeText = truncateCloseReason(text)
		c.mu.Unlock()

		c.m.observeDrop(reason)
		close(c.quit)
	})
}

// closing reports whether a closure has been requested. It is what the hub
// checks before doing work on a connection's behalf.
func (c *conn) closing() bool {
	select {
	case <-c.quit:
		return true
	default:
		return false
	}
}

// wait blocks until serve has returned. The shutdown drain uses it (D11).
func (c *conn) wait() <-chan struct{} { return c.done }

// serve runs the connection until it ends, then releases everything it owns.
//
// It blocks, and it must: the caller is the HTTP handler, and returning from a
// handler while its hijacked connection is still in use is undefined.
func (c *conn) serve(ctx context.Context) {
	defer close(c.done)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	writerDone := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		defer close(writerDone)
		c.writeLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		c.pingLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		// The close frame goes out only once the writer has stopped, so the
		// frames the drain wrote are not truncated by it. Close also unblocks
		// readLoop, which is the only thing that can.
		<-writerDone
		status, text := c.closeSpec()
		_ = c.ws.Close(status, text)
	}()

	c.readLoop(ctx)

	// readLoop returned. Either the peer went away or something already asked
	// for a closure; this call is a no-op in the second case.
	c.requestClose(DropReadError, websocket.StatusNormalClosure, "")

	// Wait for the drain BEFORE cancelling, or the last frames — the `error`
	// frame explaining a protocol violation, the going-away frame — would be
	// written on a cancelled context and lost.
	<-writerDone
	cancel()
	wg.Wait()

	_ = c.ws.CloseNow()
	c.hub.unregister(ctx, c)

	c.mu.Lock()
	reason := c.closeReason
	c.mu.Unlock()
	c.log.Info("connection finished",
		slog.String("reason", string(reason)),
		slog.Uint64("frames", c.lastSeq()),
	)
}

// lastSeq reports how many frames were assigned to this connection.
func (c *conn) lastSeq() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seq.last()
}

// -----------------------------------------------------------------------------
// The loops
// -----------------------------------------------------------------------------

// writeLoop drains the queue to the socket.
//
// One write at a time, each bounded by Options.WriteTimeout — which is NOT the
// listener's write deadline (options.go says why: a deadline on the whole
// response severs a long-lived stream, so httpx.ServerOptions.WriteTimeout is
// negative for this service and this is the per-write budget applied instead).
//
// A failed write ends the connection. There is no retry: the WebSocket framing
// is a byte stream, so a partial write has already corrupted the frame boundary
// and the only correct recovery is a new connection.
func (c *conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.quit:
			c.drain(ctx)
			return
		case out := <-c.send:
			if !c.write(ctx, out) {
				return
			}
		}
	}
}

// drain writes whatever is already queued after a closure has been requested,
// so the frame that explains the closure is not the one that gets lost.
//
// It is bounded twice over: by the queue, which is bounded by construction, and
// by one WriteTimeout per frame against a peer that has stopped reading. It
// takes nothing new — the queue is closed to new frames the instant
// requestClose sets `closed`.
func (c *conn) drain(ctx context.Context) {
	for {
		select {
		case out := <-c.send:
			if !c.write(ctx, out) {
				return
			}
		default:
			return
		}
	}
}

// write puts one frame on the socket and reports whether the loop may continue.
func (c *conn) write(ctx context.Context, out outbound) bool {
	c.m.observeWriteDelay(c.now().Sub(out.enqueued))

	wctx, cancel := context.WithTimeout(ctx, c.opts.WriteTimeout)
	err := c.ws.Write(wctx, websocket.MessageText, out.payload)
	cancel()
	if err == nil {
		return true
	}

	if ctx.Err() != nil {
		// The process is shutting down, not the client failing. Attributing it
		// to write_error would make every deploy look like a fanout regression,
		// which is precisely what DropShutdown exists to prevent.
		return false
	}
	c.log.Debug("socket write failed",
		slog.Uint64("seq", out.seq),
		slog.String("kind", string(out.kind)),
		slog.String("error", err.Error()))
	c.requestClose(DropWriteError, websocket.StatusInternalError, "write failed")
	return false
}

// pingLoop is the heartbeat and the idle reaper (D10).
//
// It uses (*websocket.Conn).Ping, which SENDS a ping and BLOCKS until the
// matching pong arrives or the context expires. That is the whole mechanism:
// hand-rolling pong tracking would mean registering a callback, correlating
// payloads and inventing a timer, and the library already does all three
// correctly. The consequence to remember is that Ping only completes while
// something is reading the socket — the pong is delivered by the read path — so
// this loop is correct ONLY because readLoop is running concurrently. It is,
// for the whole life of the connection.
//
// A ping that does not come back inside PongTimeout reaps the connection as
// idle_timeout. Options.Validate guarantees PongTimeout < PingInterval, so the
// decision is always made from ONE outstanding ping.
func (c *conn) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(c.opts.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.quit:
			return
		case <-ticker.C:
		}

		pctx, cancel := context.WithTimeout(ctx, c.opts.PongTimeout)
		err := c.ws.Ping(pctx)
		cancel()

		if err == nil {
			// The socket is alive, so the durable subscription set's lease is
			// worth refreshing. Doing it here rather than on its own timer ties
			// the lease to evidence of liveness instead of to the passage of
			// time (D6).
			c.hub.heartbeat(ctx, c)
			continue
		}

		if ctx.Err() != nil || c.closing() {
			// Shutdown, or a closure already under way. Not an idle client.
			return
		}
		c.log.Info("no pong within the heartbeat budget; reaping",
			slog.String("pong_timeout", c.opts.PongTimeout.String()))
		c.requestClose(DropIdleTimeout, websocket.StatusPolicyViolation, "no pong within the heartbeat budget")
		return
	}
}

// readLimitHeadroom is how far the LIBRARY's message ceiling sits above this
// gateway's own.
//
// They are deliberately not equal. coder/websocket enforces its limit by
// closing the connection with StatusMessageTooBig, which is a correct outcome
// but a mute one: the client gets a close code and no coded `error` frame
// naming the ceiling it exceeded. One byte of headroom means a frame that is
// merely over the limit is delivered to [DecodeClient], refused with
// ErrFrameTooLarge, and answered with a frame the client can read — while
// anything genuinely large is still refused by the library before this package
// allocates for it.
const readLimitHeadroom = 1

// readLoop reads client frames and dispatches them.
//
// The read has NO deadline, and that is correct rather than an oversight: a
// client that watches a board for an hour without sending anything is healthy,
// and a read deadline would reap it. Liveness is the heartbeat's job (D10), and
// half-open connections are what pingLoop exists for.
//
// Every protocol failure is FATAL to the connection. A client that cannot frame
// a request correctly cannot be assumed to have understood the answers it has
// already received, so the frame is not skipped — it is answered with a coded
// `error` frame and the connection is closed.
func (c *conn) readLoop(ctx context.Context) {
	c.ws.SetReadLimit(int64(c.opts.MaxFrameBytes) + readLimitHeadroom)

	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			c.readEnded(ctx, err)
			return
		}
		if typ != websocket.MessageText {
			// The protocol is JSON text frames (D8). A binary frame is not a
			// malformed document, it is a client speaking something else.
			c.protocolViolation(fmt.Errorf("%w: %s carries JSON text frames only",
				ErrMalformedFrame, Protocol), "")
			return
		}

		frame, err := DecodeClient(data, c.opts.MaxFrameBytes)
		if err != nil {
			c.protocolViolation(err, "")
			return
		}

		switch frame.Type {
		case ClientSubscribe:
			c.hub.subscribe(ctx, c, frame.Channels, frame.ID)
		case ClientUnsubscribe:
			c.hub.unsubscribe(ctx, c, frame.Channels, frame.ID)
		case ClientResync:
			c.hub.resync(ctx, c, frame.Channels, frame.ID, ResyncClientRequested)
		case ClientPing:
			c.sendFrame(NewPong(), frame.ID)
		}
	}
}

// readEnded classifies the end of the read loop.
//
// A close frame from the peer, a cancelled context and a closure this side
// already requested are all NORMAL and are not counted as read errors — a
// deploy that reaped ten thousand connections must not read as ten thousand
// client faults on the dashboard.
func (c *conn) readEnded(ctx context.Context, err error) {
	switch {
	case c.closing():
		return
	case ctx.Err() != nil:
		c.requestClose(DropShutdown, websocket.StatusGoingAway, "server going away")
	case websocket.CloseStatus(err) != -1:
		// The peer sent a close frame. That is a client leaving, not failing.
		c.requestClose(DropReadError, websocket.StatusNormalClosure, "")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		c.requestClose(DropShutdown, websocket.StatusGoingAway, "server going away")
	default:
		c.log.Debug("socket read failed", slog.String("error", err.Error()))
		c.requestClose(DropReadError, websocket.StatusInternalError, "read failed")
	}
}

// protocolViolation answers a refused frame and closes the connection.
//
// The message is FIXED TEXT chosen from the code, never the error's own string:
// the error is built from untrusted input, and this text is rendered straight
// back into a frame and into a browser console. protocol.go's ErrorCodeFor is
// the classification, and it switches over sentinels rather than over text.
func (c *conn) protocolViolation(err error, requestID string) {
	code := ErrorCodeFor(err)
	c.sendFrame(NewError(code, protocolErrorText(code)), requestID)
	c.log.Info("refusing a client frame",
		slog.String("code", string(code)),
		slog.String("error", err.Error()))
	c.requestClose(DropReasonFor(err), websocket.StatusPolicyViolation, string(code))
}

// protocolErrorText is the human-readable half of a coded error.
//
// Every string is a constant. None of them contains anything the client sent,
// and none of them names an internal cause — errors.go states the rule at
// ErrInvalidCredential and it holds for every code here: a distinction the
// client can observe is a distinction the client can probe.
func protocolErrorText(code ErrorCode) string {
	switch code {
	case CodeMalformedFrame:
		return "frame did not decode as a " + Protocol + " client frame"
	case CodeUnknownType:
		return "unknown frame type; supported types are " + clientKindList()
	case CodeFrameTooLarge:
		return "frame exceeds this gateway's size limit"
	case CodeInvalidChannel:
		return "no channel in the request could be parsed"
	case CodeChannelLimit:
		return "this connection is already holding the maximum number of channels"
	case CodeUnauthorized:
		return "the presented credential was refused; present a " +
			BearerSubprotocolPrefix + "<token> subprotocol offer or an Authorization: Bearer header"
	case CodeGoingAway:
		return "this gateway is shutting down; reconnect"
	default:
		return "internal error"
	}
}

// truncateCloseReason bounds a WebSocket close reason.
//
// RFC 6455 caps a control frame's payload at 125 bytes, of which the status
// code takes two. A longer reason makes the close frame ITSELF invalid, so the
// connection would be torn down abruptly and the client would learn nothing —
// which is the opposite of what a close reason is for.
func truncateCloseReason(s string) string {
	const limit = 120
	if len(s) <= limit {
		return s
	}
	return s[:limit]
}

// -----------------------------------------------------------------------------
// Subscription set
// -----------------------------------------------------------------------------
//
// Every method below is called ONLY from inside a Store critical section
// (Attach or Exclusive). They take no lock of their own, and adding one would
// be actively wrong: it would suggest the set can be read outside that section,
// which is the disagreement between the routing table and the connection's own
// view that D2 exists to make impossible.

// holdsLocked reports whether the connection already holds ch.
func (c *conn) holdsLocked(ch Channel) bool {
	_, ok := c.channels[ch]
	return ok
}

// addLocked records a subscription and reports whether it was new.
func (c *conn) addLocked(ch Channel) bool {
	if _, ok := c.channels[ch]; ok {
		return false
	}
	c.channels[ch] = struct{}{}
	return true
}

// removeLocked drops a subscription and reports whether it was held.
func (c *conn) removeLocked(ch Channel) bool {
	if _, ok := c.channels[ch]; !ok {
		return false
	}
	delete(c.channels, ch)
	return true
}

// countLocked reports how many channels the connection holds.
func (c *conn) countLocked() int { return len(c.channels) }

// channelsLocked returns the subscription set in a stable order.
//
// Ordered because it is rendered into the `hello` frame and into a
// client-requested resync's snapshot sequence, and map iteration order is
// randomised in Go: an unordered set would put the same channels on the wire in
// a different order on every connection, which makes a golden file impossible
// and a rendering bug that depends on arrival order intermittent. state.go
// orders its snapshots for the same reason.
func (c *conn) channelsLocked() []Channel {
	out := make([]Channel, 0, len(c.channels))
	for ch := range c.channels {
		out = append(out, ch)
	}
	sortChannels(out)
	return out
}

// marketPayloads renders a snapshot's markets.
//
// The payloads are propagated BY REFERENCE to the immutable bytes the slate
// holds — never re-marshalled. state.go already copied them out of the fetch
// buffer once; copying again per subscriber would be one allocation of a whole
// market document per client, on the path CLAUDE.md §10's ten thousand
// subscribers run through.
func marketPayloads(markets []Market) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(markets))
	for _, m := range markets {
		out = append(out, m.Payload)
	}
	return out
}
