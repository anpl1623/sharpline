package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Unit tests for the SDK's WebSocket client, against a REAL WebSocket server —
// httptest plus coder/websocket's own Accept — rather than against a fake
// socket.
//
// # Why a real handshake and not a stubbed [StreamDialer]
//
// Three of the properties in this file are properties of the HANDSHAKE and are
// unreachable through the dialer seam: that the token is offered as a
// subprotocol, that it is absent from the request URL, and that a server
// selecting a different subprotocol is refused. A stub would assert that this
// package passes the right arguments to an interface it also defines, which is
// a tautology. The seam is exercised in exactly one test, where the point is
// that a custom transport works at all.
//
// # About the market payloads below
//
// They are opaque JSON documents, not plausible-looking prices, and that is
// deliberate. The client's whole contract with a market document is that it does
// not interpret it: internal/wsgw carries the pricer's bytes through unchanged
// and so does this package. A fixture shaped like a real price would imply this
// layer understands one, and would let a future change quietly start decoding
// it. Genuine pricer output travelling the real pipeline is asserted in
// test/integration/wsgw_test.go, which is the tier that has a pipeline.

// -----------------------------------------------------------------------------
// The scripted gateway
// -----------------------------------------------------------------------------

// handshake records what one upgrade request carried. It is the evidence for the
// credential-placement assertions.
type handshake struct {
	path   string
	query  string
	offers []string
	header string
}

// gateway is an httptest server speaking the sharpline.v1 wire format under a
// test's direction.
//
// It sends the `hello` frame from inside the handler, unprompted, because that
// is what the real gateway does — [Client.Stream] does not return until the
// hello has been read, so a test that waited to be handed the connection before
// sending one would deadlock against its own client.
type gateway struct {
	t      *testing.T
	server *httptest.Server

	// conns delivers each accepted connection to the test, AFTER onAccept has
	// run. Buffered so a reconnect storm does not block the handler.
	conns chan *gatewayConn

	// onAccept is what the server says first. The default is a plain hello with
	// nothing restored; a test that needs another opening — a restored session,
	// a refusal — replaces it BEFORE calling Stream.
	onAccept func(*gatewayConn)

	// subprotocol is what Accept offers. Overridden by the one test that proves
	// a mismatched selection is refused.
	subprotocol string

	mu         sync.Mutex
	handshakes []handshake
}

// gatewayConn is one accepted connection.
type gatewayConn struct {
	ws  *websocket.Conn
	seq uint64

	// received carries every client frame, decoded. Buffered because a test
	// reads it after the fact.
	received chan map[string]any
}

func newGateway(t *testing.T) *gateway {
	t.Helper()

	g := &gateway{t: t, conns: make(chan *gatewayConn, 16), subprotocol: StreamProtocol}
	g.onAccept = func(c *gatewayConn) { _ = c.writeHello() }
	g.server = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.server.Close)
	return g
}

func (g *gateway) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.handshakes = append(g.handshakes, handshake{
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		offers: append([]string(nil), r.Header.Values("Sec-WebSocket-Protocol")...),
		header: r.Header.Get("Authorization"),
	})
	g.mu.Unlock()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols: []string{g.subprotocol},
	})
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()

	c := &gatewayConn{ws: ws, received: make(chan map[string]any, 64)}
	g.onAccept(c)

	select {
	case g.conns <- c:
	default:
		// t.Error rather than t.Fatal: this runs on the server's goroutine, and
		// Fatal there would call runtime.Goexit on the wrong one.
		g.t.Error("more connections were accepted than this test expected")
		return
	}

	for {
		typ, data, err := ws.Read(context.Background())
		if err != nil {
			close(c.received)
			return
		}
		if typ != websocket.MessageText {
			g.t.Error("the client sent a binary frame; the protocol is JSON text frames")
			continue
		}
		var f map[string]any
		if err := json.Unmarshal(data, &f); err != nil {
			g.t.Errorf("the client sent a frame that is not JSON: %v", err)
			continue
		}
		c.received <- f
	}
}

// url is the ws:// address of this gateway.
func (g *gateway) url() string { return "ws" + strings.TrimPrefix(g.server.URL, "http") }

// accept waits for the next connection, which has already been sent its opening
// frame.
func (g *gateway) accept(t *testing.T) *gatewayConn {
	t.Helper()
	select {
	case c := <-g.conns:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no connection was opened within 5s")
		return nil
	}
}

// handshake returns the recorded upgrade request n.
func (g *gateway) handshake(t *testing.T, n int) handshake {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if n >= len(g.handshakes) {
		t.Fatalf("handshake %d was never made; %d were", n, len(g.handshakes))
	}
	return g.handshakes[n]
}

// write stamps a sequence number onto a frame and sends it. It returns the
// error rather than failing a test, because it is also called from the server's
// own goroutine where t.Fatal is not available.
func (c *gatewayConn) write(seq uint64, frame map[string]any) error {
	full := map[string]any{"seq": seq, "ts": time.Now().UTC().Format(time.RFC3339Nano)}
	for k, v := range frame {
		full[k] = v
	}
	data, err := json.Marshal(full)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	c.seq = seq
	return nil
}

// writeHello sends the opening frame with nothing restored.
func (c *gatewayConn) writeHello(restored ...string) error {
	if restored == nil {
		restored = []string{}
	}
	return c.write(c.seq+1, map[string]any{
		"type":              "hello",
		"connection_id":     fmt.Sprintf("conn-%p", c),
		"protocol":          StreamProtocol,
		"heartbeat_seconds": 20,
		"session_id":        "sess-1",
		"resumed":           len(restored) > 0,
		"authenticated":     false,
		"channels":          restored,
	})
}

// send writes one frame from the TEST's goroutine, stamping the next sequence
// number.
func (c *gatewayConn) send(t *testing.T, frame map[string]any) uint64 {
	t.Helper()
	return c.sendAt(t, c.seq+1, frame)
}

// sendAt writes one frame with an explicit sequence number, which is how a hole
// is manufactured.
func (c *gatewayConn) sendAt(t *testing.T, seq uint64, frame map[string]any) uint64 {
	t.Helper()
	if err := c.write(seq, frame); err != nil {
		t.Fatalf("write the server frame: %v", err)
	}
	return seq
}

// await returns the next client frame, requiring it to be of the given type.
func (c *gatewayConn) await(t *testing.T, want string) map[string]any {
	t.Helper()
	select {
	case f, ok := <-c.received:
		if !ok {
			t.Fatalf("the connection ended before a %q frame arrived", want)
		}
		if got, _ := f["type"].(string); got != want {
			t.Fatalf("client sent a %q frame, want %q (%v)", got, want, f)
		}
		return f
	case <-time.After(5 * time.Second):
		t.Fatalf("no %q frame arrived within 5s", want)
		return nil
	}
}

// silent asserts that no client frame arrives within a short window. It is how
// "only one resync was requested" is proved: the absence has to be observed,
// because a second request would otherwise simply be read by a later await.
func (c *gatewayConn) silent(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case f, ok := <-c.received:
		if ok {
			t.Fatalf("the client sent an unexpected frame: %v", f)
		}
	case <-time.After(within):
	}
}

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

// testClient builds a Client pointed at addr. The base URL is only used to
// derive a default stream address; every test that cares supplies StreamOptions.URL.
func testClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: baseURL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// nextEvent reads one event with a deadline, failing the test rather than
// hanging the suite.
func nextEvent(t *testing.T, s *Stream) StreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	return ev
}

// awaitEvent reads events until one of the wanted kind arrives.
func awaitEvent(t *testing.T, s *Stream, want StreamEventKind) StreamEvent {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		ev, err := s.Next(ctx)
		cancel()
		if err != nil {
			t.Fatalf("waiting for a %s event: %v", want, err)
		}
		if ev.Kind == want {
			return ev
		}
	}
	t.Fatalf("no %s event arrived within 5s", want)
	return StreamEvent{}
}

// -----------------------------------------------------------------------------
// Credential placement
// -----------------------------------------------------------------------------

// The token is the one value in this package that must never reach a URL. This
// is the assertion that says so, and it reads the actual bytes of the actual
// upgrade request rather than the arguments passed to an interface.
func TestTheTokenIsOfferedAsASubprotocolAndNeverAppearsInTheURL(t *testing.T) {
	t.Parallel()

	const token = "aaa.bbb.ccc"

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), Token: token})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	g.accept(t)
	if ev := nextEvent(t, s); ev.Kind != StreamEventHello {
		t.Fatalf("first event is %s, want %s", ev.Kind, StreamEventHello)
	}

	h := g.handshake(t, 0)
	if h.query != "" {
		t.Errorf("the upgrade request carried a query string %q; a token in a URL is written to "+
			"every access log in the path", h.query)
	}
	if strings.Contains(h.path, token) {
		t.Errorf("the token appears in the request path %q", h.path)
	}
	if h.header != "" {
		t.Errorf("an Authorization header was sent (%q); this client uses the subprotocol so that "+
			"the browser and the Go SDK exercise one credential path", h.header)
	}

	offered := strings.Join(h.offers, ",")
	if !strings.Contains(offered, StreamProtocol) {
		t.Errorf("Sec-WebSocket-Protocol %q does not offer %q", offered, StreamProtocol)
	}
	if !strings.Contains(offered, bearerSubprotocolPrefix+token) {
		t.Errorf("Sec-WebSocket-Protocol %q does not carry the bearer offer", offered)
	}
}

// A URL carrying a credential is refused BEFORE a connection is attempted. The
// gateway refuses the same thing, but only after the URL has already been
// written to whatever access log sits in front of it.
func TestAStreamURLCarryingATokenIsRefusedBeforeAnythingIsDialled(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	for _, param := range []string{"token", "access_token", "jwt", "authorization"} {
		t.Run(param, func(t *testing.T) {
			_, err := c.Stream(t.Context(), StreamOptions{
				URL: g.url() + "/ws?" + param + "=aaa.bbb.ccc",
			})
			if !errors.Is(err, ErrTokenInURL) {
				t.Fatalf("Stream(?%s=…) error = %v, want ErrTokenInURL", param, err)
			}
			if strings.Contains(err.Error(), "aaa.bbb.ccc") {
				t.Errorf("the refusal quotes the credential: %v", err)
			}
		})
	}

	g.mu.Lock()
	made := len(g.handshakes)
	g.mu.Unlock()
	if made != 0 {
		t.Errorf("%d upgrade requests were made; the refusal must happen before the dial", made)
	}
}

// A failed handshake must not put the credential into the error a caller logs.
func TestADialFailureErrorNeverContainsTheToken(t *testing.T) {
	t.Parallel()

	const token = "secret.token.value"

	// A plain HTTP server that never upgrades.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, srv.URL)
	_, err := c.Stream(t.Context(), StreamOptions{
		URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Token: token,
	})
	if err == nil {
		t.Fatal("Stream succeeded against a server that refuses to upgrade")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the dial error carries the credential: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Sequence gaps
// -----------------------------------------------------------------------------

// CLAUDE.md §5: "Every message carries a monotonic sequence number; a gap
// triggers client resync." This is the client half, and it does not depend on
// the server admitting anything.
func TestASequenceJumpIsDetectedAndAnsweredWithAResyncRequest(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	conn.send(t, map[string]any{ // seq 2
		"type": "delta", "channel": "market:m1", "market": json.RawMessage(`{"opaque":1}`),
	})
	awaitEvent(t, s, StreamEventDelta)

	// seq 3 and 4 never arrive.
	conn.sendAt(t, 5, map[string]any{
		"type": "delta", "channel": "market:m1", "market": json.RawMessage(`{"opaque":2}`),
	})

	gap := awaitEvent(t, s, StreamEventGap)
	if gap.Gap == nil {
		t.Fatal("the gap event carries no StreamGap")
	}
	if gap.Gap.From != 3 || gap.Gap.To != 4 || gap.Gap.Dropped != 2 {
		t.Errorf("gap = %+v, want frames 3-4 (2 dropped)", *gap.Gap)
	}
	if gap.Gap.ServerReported {
		t.Error("the gap is marked as server-reported; nothing on the wire said so")
	}

	req := conn.await(t, "resync")
	if chans, ok := req["channels"]; ok && chans != nil {
		if list, _ := chans.([]any); len(list) != 0 {
			t.Errorf("the resync request named channels %v; a sequence number says a frame was "+
				"lost and says nothing about which channel it was for", list)
		}
	}

	// The delta that revealed the hole is still delivered: it is a real frame and
	// discarding it would widen the hole the client just reported.
	if ev := awaitEvent(t, s, StreamEventDelta); string(ev.Market) != `{"opaque":2}` {
		t.Errorf("market = %s, want the frame that revealed the gap", ev.Market)
	}
}

// A resync frame and a locally detected gap lead to the same recovery and are
// different observations. Conflating them would lose the distinction between
// "the gateway shed load" and "something between us dropped frames".
func TestAServerSentResyncIsDistinguishedFromALocallyDetectedGap(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	// In-sequence, so nothing is detected locally: the server is volunteering
	// that it discarded frames it never assigned to the wire.
	conn.send(t, map[string]any{
		"type": "resync", "reason": "slow_consumer",
		"dropped": 12, "from_seq": 100, "to_seq": 111,
	})

	ev := awaitEvent(t, s, StreamEventResync)
	if ev.Reason != "slow_consumer" {
		t.Errorf("reason = %q, want slow_consumer", ev.Reason)
	}
	if ev.Gap == nil || !ev.Gap.ServerReported {
		t.Fatalf("resync gap = %+v, want ServerReported", ev.Gap)
	}
	if ev.Gap.Dropped != 12 || ev.Gap.From != 100 || ev.Gap.To != 111 {
		t.Errorf("gap = %+v, want the server's own account of the hole", *ev.Gap)
	}

	conn.await(t, "resync")
}

// A slow-consumer discard produces BOTH observations from one frame: the resync
// frame's sequence number continues from the highest already assigned, so the
// hole is visible on the wire and the frame that reveals it also explains it.
// Exactly one recovery request must follow.
func TestOnlyOneResyncIsRequestedPerHole(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	// This is the shape internal/wsgw produces on overflow: the queued frames
	// 2..40 were discarded and the resync is stamped 41.
	conn.sendAt(t, 41, map[string]any{
		"type": "resync", "reason": "slow_consumer",
		"dropped": 39, "from_seq": 2, "to_seq": 40,
	})

	gap := awaitEvent(t, s, StreamEventGap)
	if gap.Gap.ServerReported {
		t.Error("the locally detected gap claims to be server-reported")
	}
	resync := awaitEvent(t, s, StreamEventResync)
	if !resync.Gap.ServerReported {
		t.Error("the server-sent resync is not marked as server-reported")
	}

	conn.await(t, "resync")
	conn.silent(t, 250*time.Millisecond)

	// A snapshot clears the pending state, so the NEXT hole is answered again.
	conn.send(t, map[string]any{
		"type": "snapshot", "channel": "league:nfl",
		"markets": []json.RawMessage{}, "complete": true,
	})
	awaitEvent(t, s, StreamEventSnapshot)

	conn.sendAt(t, 60, map[string]any{
		"type": "delta", "channel": "market:m1", "market": json.RawMessage(`{"opaque":3}`),
	})
	awaitEvent(t, s, StreamEventGap)
	conn.await(t, "resync")
}

// A sequence number that goes BACKWARDS is not a hole; it is a server that is
// not speaking this protocol, and reconnecting into one is an unbounded loop of
// the same failure.
func TestSequenceGoingBackwardsEndsTheStreamRatherThanReconnecting(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)
	conn.sendAt(t, 9, map[string]any{
		"type": "delta", "channel": "market:m1", "market": json.RawMessage(`{"opaque":1}`),
	})
	awaitEvent(t, s, StreamEventGap)
	awaitEvent(t, s, StreamEventDelta)

	conn.sendAt(t, 4, map[string]any{
		"type": "delta", "channel": "market:m1", "market": json.RawMessage(`{"opaque":2}`),
	})

	awaitEvent(t, s, StreamEventDisconnected)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("Next error = %v, want ErrStreamProtocol", err)
	}

	g.mu.Lock()
	made := len(g.handshakes)
	g.mu.Unlock()
	if made != 1 {
		t.Errorf("%d connections were made; a protocol failure must not be retried", made)
	}
}

// -----------------------------------------------------------------------------
// Payload propagation
// -----------------------------------------------------------------------------

// The market document is carried byte for byte from the pricer to the caller.
// internal/wsgw refuses to re-shape it and neither does this package: two
// mappings of the same facts eventually stop agreeing, and the disagreement
// shows up as a subtly wrong line rather than as a failure.
func TestMarketDocumentsAreCarriedByteForByte(t *testing.T) {
	t.Parallel()

	// Deliberately awkward: key order that a re-marshal would normalise, a
	// number a float round trip would rewrite, and a nested object.
	const document = `{"zulu":1,"alpha":{"n":0.1000000000000000055511151231257827},"m":9007199254740993}`

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	conn.send(t, map[string]any{
		"type": "snapshot", "channel": "event:e1",
		"markets":  []json.RawMessage{json.RawMessage(document)},
		"complete": true,
	})
	snap := awaitEvent(t, s, StreamEventSnapshot)
	if len(snap.Markets) != 1 {
		t.Fatalf("snapshot carries %d markets, want 1", len(snap.Markets))
	}
	if string(snap.Markets[0]) != document {
		t.Errorf("snapshot market = %s\nwant                    %s", snap.Markets[0], document)
	}

	conn.send(t, map[string]any{
		"type": "delta", "channel": "event:e1", "market": json.RawMessage(document),
	})
	delta := awaitEvent(t, s, StreamEventDelta)
	if string(delta.Market) != document {
		t.Errorf("delta market = %s\nwant              %s", delta.Market, document)
	}
	if delta.IsRemoval() {
		t.Error("an update delta reports itself as a removal")
	}

	conn.send(t, map[string]any{
		"type": "delta", "channel": "event:e1", "removed": "mkt-gone",
	})
	tomb := awaitEvent(t, s, StreamEventDelta)
	if !tomb.IsRemoval() || tomb.Removed != "mkt-gone" {
		t.Errorf("removal delta = %+v, want removed=mkt-gone", tomb)
	}
}

// An empty snapshot is a CORRECT answer for a channel that holds nothing — a
// league with no scheduled events — and it must not reach a caller as a nil
// slice they have to special-case.
func TestAnEmptySnapshotIsAnEventWithAnEmptySlice(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	conn.send(t, map[string]any{
		"type": "snapshot", "channel": "league:empty", "markets": []json.RawMessage{},
		"complete": true,
	})
	ev := awaitEvent(t, s, StreamEventSnapshot)
	if ev.Markets == nil {
		t.Fatal("markets is nil; an empty snapshot must arrive as an empty slice")
	}
	if len(ev.Markets) != 0 {
		t.Fatalf("markets has %d entries, want none", len(ev.Markets))
	}
}

// -----------------------------------------------------------------------------
// Reconnection
// -----------------------------------------------------------------------------

// The backoff is asserted on the SCHEDULE the stream reports rather than on wall
// clock time. RetryIn is the exact value the policy computed, so with a fixed
// jitter the assertion is deterministic — and a test that measured sleeps would
// be a test that fails on a loaded machine.
func TestReconnectBackoffGrowsAndIsCapped(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	const (
		base = 8 * time.Millisecond
		cap_ = 20 * time.Millisecond
	)

	// Every connection is answered with a hello and then dropped, so the client
	// never holds a healthy connection and the attempt counter never resets.
	g.onAccept = func(conn *gatewayConn) {
		_ = conn.writeHello()
		_ = conn.ws.Close(websocket.StatusGoingAway, "bye")
	}

	s, err := c.Stream(t.Context(), StreamOptions{
		URL: g.url(),
		Backoff: StreamBackoff{
			BaseDelay:   base,
			MaxDelay:    cap_,
			MaxAttempts: 4,
			// Full jitter draws uniformly from [0, window); pinning the draw
			// makes the window itself observable.
			Jitter: func() float64 { return 0.5 },
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	var waits []time.Duration
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		ev, err := s.Next(ctx)
		cancel()
		if err != nil {
			if !errors.Is(err, ErrStreamReconnectExhausted) {
				t.Fatalf("Next error = %v, want ErrStreamReconnectExhausted", err)
			}
			break
		}
		if ev.Kind == StreamEventDisconnected && ev.RetryIn > 0 {
			waits = append(waits, ev.RetryIn)
		}
	}

	if len(waits) < 3 {
		t.Fatalf("observed %d reconnect waits (%v), want at least 3", len(waits), waits)
	}
	for i, w := range waits {
		if w > cap_ {
			t.Errorf("wait %d is %s, above the %s ceiling", i+1, w, cap_)
		}
	}
	// Full jitter at a pinned draw of 0.5 makes each wait exactly half its
	// window, and the window doubles until it saturates.
	if waits[0] != base/2 {
		t.Errorf("first wait is %s, want %s (half of the %s base window)", waits[0], base/2, base)
	}
	if waits[1] != base {
		t.Errorf("second wait is %s, want %s (half of the doubled window)", waits[1], base)
	}
	if waits[2] != cap_/2 {
		t.Errorf("third wait is %s, want %s (half of the saturated %s window)", waits[2], cap_/2, cap_)
	}
}

// A reconnection restores the subscription the caller asked for. Without this a
// stream that survived a deploy would be a stream that quietly stopped
// delivering.
func TestAReconnectionResubscribesTheDesiredChannels(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{
		URL:      g.url(),
		Channels: []string{"league:nfl"},
		Backoff:  StreamBackoff{BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	first := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	sub := first.await(t, "subscribe")
	assertChannels(t, sub, "league:nfl")

	if err := s.Subscribe(t.Context(), "event:e1"); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	assertChannels(t, first.await(t, "subscribe"), "event:e1")

	_ = first.ws.Close(websocket.StatusGoingAway, "deploy")

	second := g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	// Both channels, in the order they were asked for, on ONE frame.
	assertChannels(t, second.await(t, "subscribe"), "league:nfl", "event:e1")
}

// The server restores a session's channels from Redis when a client reconnects
// onto another replica. Re-subscribing to those would be answered with a
// `duplicate` rejection — correct, and indistinguishable on a dashboard from a
// client bug.
func TestRestoredChannelsAreNotSubscribedAgain(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	g.onAccept = func(conn *gatewayConn) { _ = conn.writeHello("league:nfl") }
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{
		URL:      g.url(),
		Channels: []string{"league:nfl", "event:e1"},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)

	ev := awaitEvent(t, s, StreamEventHello)
	if ev.Hello == nil || !ev.Hello.Resumed {
		t.Fatalf("hello = %+v, want Resumed", ev.Hello)
	}
	if len(ev.Hello.Channels) != 1 || ev.Hello.Channels[0] != "league:nfl" {
		t.Errorf("restored channels = %v, want [league:nfl]", ev.Hello.Channels)
	}

	assertChannels(t, conn.await(t, "subscribe"), "event:e1")
}

// A stream with reconnection disabled ends at the first disconnection, and says
// why in the same call that reports the end.
func TestNoReconnectEndsTheStreamAtTheFirstDisconnection(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), NoReconnect: true})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	conn := g.accept(t)
	awaitEvent(t, s, StreamEventHello)
	_ = conn.ws.Close(websocket.StatusGoingAway, "bye")

	awaitEvent(t, s, StreamEventDisconnected)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Next(ctx); err == nil {
		t.Fatal("Next succeeded after the stream ended")
	}

	g.mu.Lock()
	made := len(g.handshakes)
	g.mu.Unlock()
	if made != 1 {
		t.Errorf("%d connections were made, want 1", made)
	}
}

// -----------------------------------------------------------------------------
// Protocol negotiation and lifecycle
// -----------------------------------------------------------------------------

// There is no default protocol. A server that selects something else is refused
// rather than spoken to: an unversioned connection is one whose frame shapes can
// change under a client with no way to notice.
func TestAServerThatSelectsAnotherSubprotocolIsRefused(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	g.subprotocol = "sharpline.v2"
	c := testClient(t, g.server.URL)

	_, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("Stream error = %v, want ErrStreamProtocol", err)
	}
}

// The gateway refuses a credential over the socket rather than with a status
// code, because a browser's WebSocket API gives a page nothing from a non-101
// response. The SDK must therefore read that refusal off the first frame.
func TestAnErrorFrameInsteadOfHelloIsReportedFromStream(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	g.onAccept = func(conn *gatewayConn) {
		_ = conn.write(1, map[string]any{
			"type": "error", "code": "unauthorized",
			"message": "present the token as a sharpline.bearer.<token> subprotocol offer",
		})
		_ = conn.ws.Close(websocket.StatusPolicyViolation, "unauthorized")
	}
	c := testClient(t, g.server.URL)

	_, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), Token: "aaa.bbb.ccc"})
	if !errors.Is(err, ErrStreamProtocol) {
		t.Fatalf("Stream error = %v, want ErrStreamProtocol", err)
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("the error does not carry the server's code: %v", err)
	}
	if strings.Contains(err.Error(), "aaa.bbb.ccc") {
		t.Errorf("the error carries the credential: %v", err)
	}
}

// Close is idempotent, releases the goroutine, and reports itself through Next
// rather than leaving a caller to consult a second accessor.
func TestCloseEndsNextWithErrStreamClosed(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url()})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Next after Close = %v, want ErrStreamClosed", err)
	}
	if err := s.Err(); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Err after Close = %v, want ErrStreamClosed", err)
	}
}

// The dialer seam exists so a caller can substitute a transport this package
// does not model. One test proves it is actually used, which is all a seam
// needs.
func TestACustomDialerIsUsed(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	spy := &recordingDialer{inner: &websocketDialer{readLimit: DefaultStreamReadLimit}}
	s, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), Dialer: spy, Token: "aaa.bbb.ccc"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	g.accept(t)
	awaitEvent(t, s, StreamEventHello)

	spy.mu.Lock()
	defer spy.mu.Unlock()
	if spy.calls != 1 {
		t.Fatalf("the dialer was called %d times, want 1", spy.calls)
	}
	if len(spy.offers) < 2 || spy.offers[0] != StreamProtocol {
		t.Fatalf("offers = %v, want %q first then the bearer offer", spy.offers, StreamProtocol)
	}
	if !strings.HasPrefix(spy.offers[1], bearerSubprotocolPrefix) {
		t.Errorf("second offer %q is not a bearer offer", spy.offers[1])
	}
}

type recordingDialer struct {
	inner StreamDialer

	mu     sync.Mutex
	calls  int
	offers []string
}

func (d *recordingDialer) DialStream(ctx context.Context, rawURL string, subprotocols []string) (StreamSocket, error) {
	d.mu.Lock()
	d.calls++
	d.offers = append([]string(nil), subprotocols...)
	d.mu.Unlock()
	return d.inner.DialStream(ctx, rawURL, subprotocols)
}

// -----------------------------------------------------------------------------
// URL derivation and validation
// -----------------------------------------------------------------------------

func TestTheStreamAddressIsDerivedFromTheClientBaseURL(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		base string
		want string
	}{
		{"http://sharpline.example", "ws://sharpline.example/ws"},
		{"https://sharpline.example", "wss://sharpline.example/ws"},
		{"https://sharpline.example/api/v1", "wss://sharpline.example/ws"},
		{"http://127.0.0.1:8080", "ws://127.0.0.1:8080/ws"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			c := testClient(t, tc.base)
			got, err := c.streamURL("")
			if err != nil {
				t.Fatalf("streamURL: %v", err)
			}
			if got != tc.want {
				t.Errorf("streamURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnEmptyOrOverlongChannelIsRefusedLocally(t *testing.T) {
	t.Parallel()

	g := newGateway(t)
	c := testClient(t, g.server.URL)

	if _, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), Channels: []string{"  "}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Stream with a blank channel = %v, want ErrInvalidOptions", err)
	}
	long := "market:" + strings.Repeat("x", maxStreamChannelLen)
	if _, err := c.Stream(t.Context(), StreamOptions{URL: g.url(), Channels: []string{long}}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Stream with an overlong channel = %v, want ErrInvalidOptions", err)
	}
}

// assertChannels checks a client frame's `channels` array.
func assertChannels(t *testing.T, frame map[string]any, want ...string) {
	t.Helper()

	raw, ok := frame["channels"].([]any)
	if !ok {
		t.Fatalf("frame %v carries no channels array", frame)
	}
	got := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		got = append(got, s)
	}
	if len(got) != len(want) {
		t.Fatalf("channels = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("channels = %v, want %v", got, want)
		}
	}
}
