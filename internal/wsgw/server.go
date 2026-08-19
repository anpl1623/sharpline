// The upgrade handler: the one HTTP route this service serves, and the four
// refusals in front of it.
//
// It is mounted by cmd/stream as `GET /ws` on the httpx server's mux, which is
// the path deploy/proxy/Caddyfile forwards. Everything else on that listener is
// the operational surface — /healthz, /readyz, /metrics — and is unreachable
// from outside the container network.
//
// # A WebSocket is NOT protected by CORS, so the origin check here is the control
//
// This is the single most misunderstood thing about the handshake and it is
// worth stating plainly. The same-origin policy does not apply to
// `new WebSocket()`: a page on evil.example can open a WebSocket to this
// service, the browser will attach the user's cookies, and no preflight and no
// CORS header will stop it. The `Origin` header is sent, and CHECKING IT IS THE
// ONLY DEFENCE — which is exactly why coder/websocket verifies it by default
// and makes disabling that an option named InsecureSkipVerify.
//
// So the default here is same-origin only. CLAUDE.md §7 says the browser talks
// to this service through the proxy — "the browser talks to the API through the
// reverse proxy, never to a container hostname" — so the page and the socket
// share an origin and the default is also the correct production setting.
// [ServerOptions.AllowedOrigins] exists for a deployment that genuinely serves
// the frontend from another host, and it takes patterns rather than a wildcard
// because coder/websocket deliberately refuses `*` in that list.
//
// # No compression, and it is the proxy's reasoning
//
// deploy/proxy/Caddyfile scopes `encode` to every route EXCEPT /ws, and says
// why: "the stream protocol is snapshot-then-delta with per-message framing,
// and an HTTP-level encoder in front of an upgraded connection either buffers
// frames (adding staleness — the headline SLO, CLAUDE.md §9) or is simply dead
// weight."
//
// permessage-deflate at this layer has the same problem in a sharper form. With
// context takeover it holds a flate window PER CONNECTION, which at CLAUDE.md
// §10's ten thousand subscribers is memory measured in gigabytes; without it,
// each small delta is compressed from a cold dictionary and mostly grows. The
// frames are small JSON documents on a local network, and the thing being
// optimised is age, not bytes. So it is disabled explicitly rather than left to
// the library's default — the default is already CompressionDisabled, and
// stating it means a library change cannot quietly turn it on.
package wsgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"

	"github.com/coder/websocket"
)

// DefaultMaxConnections is the per-process ceiling on live connections.
//
// CLAUDE.md §10 states the target as 10k concurrent subscribers on one node, so
// a ceiling at or below that would cap the thing the load test exists to
// demonstrate. 16384 is the next power of two with room to spare, and the point
// of having a ceiling at all is not to be reached: it is to convert an
// unbounded accept loop — which ends as an OOM kill that takes every connected
// client down with it — into a bounded refusal that sheds the marginal client
// and keeps the ones already being served.
const DefaultMaxConnections = 16384

// Route is the path cmd/stream mounts this handler on. It is declared here so
// the entrypoint and the proxy configuration cannot drift apart silently;
// deploy/proxy/Caddyfile matches `/ws` and `/ws/*`.
const Route = "/ws"

// ServerOptions configures the upgrade handler.
type ServerOptions struct {
	// Hub is the fanout this handler admits connections into. Required.
	Hub *Hub

	// AllowedOrigins lists host patterns permitted to open a connection, in
	// addition to the request's own host (which is always allowed).
	//
	// EMPTY MEANS SAME-ORIGIN ONLY, which is the correct production setting for
	// this deployment (see the file comment) and the safe default for one that
	// is misconfigured. Patterns are matched with path.Match by
	// coder/websocket; `*` is deliberately not accepted by it, and this package
	// does not add a way around that.
	AllowedOrigins []string

	// MaxConnections caps live connections on this process. Zero means
	// [DefaultMaxConnections]; negative is a configuration error.
	MaxConnections int

	// TrustedProxies is the set of peers whose X-Real-IP header may be
	// believed. Empty means none, and the peer address is used.
	//
	// It implements only the X-Real-IP half of the discipline
	// internal/httpapi/middleware/clientip.go applies, and that asymmetry is
	// deliberate rather than an omission — see [Server.clientAddr].
	TrustedProxies []netip.Prefix
}

// Server is the WebSocket upgrade handler.
//
// It holds no state beyond the live-connection count: the routing table, the
// slate and every connection belong to the [Hub].
type Server struct {
	hub    *Hub
	opts   Options
	accept *websocket.AcceptOptions
	m      *Metrics
	log    *slog.Logger

	maxConns int
	trusted  []netip.Prefix

	// live is the in-flight connection count behind the process ceiling. It is
	// separate from sharpline_ws_connections_active, which is the HPA's input
	// and must not be reused as a control variable: a gauge that something
	// decides from is a gauge somebody will eventually reset for a diagnostic.
	live atomic.Int64
}

var _ http.Handler = (*Server)(nil)

// NewServer builds the upgrade handler.
//
// It returns an error rather than panicking (CLAUDE.md §12), and it builds the
// AcceptOptions ONCE: they are immutable and shared by every handshake, so a
// per-request allocation of the same struct would be pure waste on the one path
// that runs ten thousand times.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Hub == nil {
		return nil, fmt.Errorf("%w: Hub is nil", ErrInvalidOptions)
	}
	if opts.MaxConnections < 0 {
		return nil, fmt.Errorf("%w: MaxConnections is %d; a negative ceiling is not a disable",
			ErrInvalidOptions, opts.MaxConnections)
	}
	for _, p := range opts.AllowedOrigins {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("%w: AllowedOrigins contains an empty pattern", ErrInvalidOptions)
		}
		if p == "*" {
			return nil, fmt.Errorf("%w: AllowedOrigins may not contain \"*\"; a WebSocket is not "+
				"protected by CORS, so the origin check is the only control there is",
				ErrInvalidOptions)
		}
	}

	gw := opts.Hub.Options()
	maxConns := opts.MaxConnections
	if maxConns == 0 {
		maxConns = DefaultMaxConnections
	}

	return &Server{
		hub:  opts.Hub,
		opts: gw,
		accept: &websocket.AcceptOptions{
			// Only "sharpline.v1" is ever selected. The bearer offer is a
			// credential carrier, not a protocol, and echoing it would write the
			// client's access token into the handshake RESPONSE — see
			// SelectSubprotocol.
			Subprotocols: []string{Protocol},

			// Stated rather than left to the zero value. It is the option whose
			// name is a warning, and a reader deserves to see that this service
			// did not take it.
			InsecureSkipVerify: false,
			OriginPatterns:     append([]string(nil), opts.AllowedOrigins...),

			// See the file comment.
			CompressionMode: websocket.CompressionDisabled,
		},
		m:        gw.Metrics,
		log:      gw.Logger.With(slog.String("component", "wsgw.server")),
		maxConns: maxConns,
		trusted:  append([]netip.Prefix(nil), opts.TrustedProxies...),
	}, nil
}

// ServeHTTP upgrades a request and serves the connection until it ends.
//
// # The order of the checks, and what each one costs
//
//  1. SHUTTING DOWN and the CONNECTION CEILING are answered with 503 and
//     nothing is upgraded. A gateway that accepted the handshake and then
//     immediately closed would spend the handshake's cost to say no, and a
//     client with a jittered reconnect would treat a 1001 close as "try again
//     shortly" rather than as "this replica is full" — a 503 is what a load
//     balancer's passive health check already understands.
//
//  2. NO "sharpline.v1" OFFER is answered with 400, before the upgrade, because
//     there is no version to speak the refusal in. There is no default protocol
//     (see [Protocol]): an unversioned connection is one whose frame shapes can
//     never be changed without breaking somebody silently.
//
//  3. THE UPGRADE itself. coder/websocket writes its own error response on a
//     failed handshake — a non-WebSocket request, a disallowed origin — so this
//     handler only counts it.
//
//  4. THE CREDENTIAL, and this one is refused OVER THE SOCKET rather than with
//     a status code. A browser's WebSocket API gives a page essentially nothing
//     from a non-101 response: no status, no body, one opaque error event. It
//     does give it `CloseEvent.code` and `CloseEvent.reason`. So the only way a
//     developer who put their token in the query string can actually be TOLD
//     that — which is the entire point of D5's refusal — is to upgrade and then
//     close with a policy violation and a readable reason. It is counted as
//     `rejected`, because it is this gateway's decision, even though the
//     handshake succeeded.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hub.closed.Load() {
		s.refuse(w, http.StatusServiceUnavailable, "this gateway is shutting down")
		return
	}

	if n := s.live.Add(1); n > int64(s.maxConns) {
		s.live.Add(-1)
		s.log.Warn("refusing a connection at the process ceiling",
			slog.Int("max_connections", s.maxConns))
		s.refuse(w, http.StatusServiceUnavailable, "this replica is at its connection limit")
		return
	}
	defer s.live.Add(-1)

	if _, ok := SelectSubprotocol(r); !ok {
		s.log.Debug("refusing a client that offered no protocol",
			slog.String("remote", s.clientAddr(r)))
		s.refuse(w, http.StatusBadRequest,
			"offer the "+Protocol+" subprotocol; this gateway has no default protocol")
		return
	}

	sock, err := websocket.Accept(w, r, s.accept)
	if err != nil {
		// Accept has already written a response. Counting it separately from a
		// rejection matters: upgrade_failed rising means the network or the
		// clients are broken, rejected rising means this gateway is turning
		// people away, and conflating them makes an incident harder to read.
		s.m.observeConnection(ConnectionUpgradeFailed)
		s.log.Debug("websocket upgrade failed",
			slog.String("remote", s.clientAddr(r)),
			slog.String("error", err.Error()))
		return
	}
	defer func() { _ = sock.CloseNow() }()

	ctx := r.Context()

	identity, err := s.authenticate(ctx, r)
	if err != nil {
		s.m.observeConnection(ConnectionRejected)
		s.log.Info("refusing a credential",
			slog.String("remote", s.clientAddr(r)),
			slog.String("error", err.Error()))
		s.rejectOverSocket(ctx, sock, err)
		return
	}

	c := newConn(connOptions{
		ID:        s.hub.newID(),
		SessionID: sessionKeyFor(identity, s.hub.newID),
		Resumable: identity.SessionID != "",
		Identity:  identity,
		Remote:    s.clientAddr(r),
		Socket:    sock,
		Hub:       s.hub,
		Options:   s.opts,
		Logger:    s.log,
		Now:       s.hub.now,
	})

	if err := s.hub.Register(ctx, c); err != nil {
		s.m.observeConnection(ConnectionRejected)
		_ = sock.Close(websocket.StatusGoingAway, "server going away")
		return
	}

	s.m.observeConnection(ConnectionAccepted)
	s.m.observeConnectionOpened()
	// Paired from a defer because the gauge is the HPA's input (CLAUDE.md §9):
	// a leaked increment does not merely mis-report, it scales the deployment
	// up and keeps it there.
	defer s.m.observeConnectionClosed()

	c.serve(ctx)
}

// authenticate extracts and verifies a presented credential.
//
// Three outcomes, and only one of them proceeds authenticated — auth.go's
// Authenticate states them. What this wrapper adds is nothing: it exists so the
// handler has one call rather than two and so the ORDER (query string first,
// which is a refusal) cannot be reordered by an edit here.
func (s *Server) authenticate(ctx context.Context, r *http.Request) (Identity, error) {
	cred, err := ExtractCredential(r)
	if err != nil {
		return Identity{}, err
	}
	return Authenticate(ctx, s.opts.Verifier, cred)
}

// rejectOverSocket tells an upgraded client why it is being closed, then closes
// it.
//
// The frame is written directly rather than through a [conn], because there is
// no connection: nothing has been registered, no send queue exists and no
// goroutine has been started. It still carries `"seq":1`, so a client's frame
// decoder does not need a special case for the one frame that has no sequence
// number — every frame on every connection has one, including this one, and
// that invariant is cheaper to keep than to document an exception to.
//
// The message names the SUPPORTED MECHANISMS and never why verification failed.
// errors.go states the rule at ErrInvalidCredential: a distinction the client
// can observe between expired, badly signed and unknown-subject is an oracle.
func (s *Server) rejectOverSocket(ctx context.Context, sock wsConn, cause error) {
	code := ErrorCodeFor(cause)
	if code != CodeUnauthorized {
		code = CodeUnauthorized
	}

	text := protocolErrorText(code)
	if errors.Is(cause, ErrTokenInQuery) {
		text = "an access token must not appear in the URL; present it as a " +
			BearerSubprotocolPrefix + "<token> subprotocol offer or an Authorization: Bearer header"
	}

	if frame, err := Frame(1, NewError(code, text), s.hub.now(), ""); err == nil {
		wctx, cancel := context.WithTimeout(ctx, s.opts.WriteTimeout)
		_ = sock.Write(wctx, websocket.MessageText, frame)
		cancel()
		s.m.observeSent(KindError)
	}
	_ = sock.Close(websocket.StatusPolicyViolation, truncateCloseReason(string(code)))
}

// refuse answers a request that was never upgraded.
//
// It writes plain text, not a Problem document: this is not the REST surface,
// nothing here negotiates content, and the only readers are a load balancer
// (which reads the status), curl and a developer. The status is what carries
// the meaning.
func (s *Server) refuse(w http.ResponseWriter, status int, reason string) {
	s.m.observeConnection(ConnectionRejected)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, reason)
}

// clientAddr determines the address to attribute a connection to, for LOGGING.
//
// # Why this is not internal/httpapi/middleware's implementation
//
// That package does this properly: a trusted-peer check, X-Real-IP, then an
// X-Forwarded-For walk from the RIGHT skipping trusted hops. Reusing it is not
// available — `clientAddr` there is unexported, and `ClientIPFrom` reads a
// value the API's middleware chain put in the context, which this route does
// not run through because it is mounted directly on the httpx mux.
//
// The choice was therefore between importing that package for a context lookup
// that would always miss, copying its X-Forwarded-For walk, or implementing the
// half that this deployment actually uses. It implements the half:
// deploy/proxy/Caddyfile sets `header_up X-Real-IP {remote_host}` on the /ws
// route, and `header_up` with a single value REPLACES whatever the client sent
// — so behind this proxy that header is authoritative in a way X-Forwarded-For
// is not.
//
// The omission is safe for one specific reason, and it stops being safe the
// moment that reason does: NOTHING IN THIS PACKAGE MAKES A DECISION FROM THIS
// VALUE. It is a log field. The reason middleware/clientip.go needs the full
// right-to-left walk is per-IP rate limiting (CLAUDE.md §6), where a forgeable
// address is an honour system. If this gateway ever rate-limits by address,
// this function is not the thing to extend — the correct move is to export the
// existing implementation and use it, rather than to grow a second copy of a
// security-sensitive parse.
//
// An untrusted peer's headers are never believed at all.
func (s *Server) clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, perr := netip.ParseAddr(host)
	if perr != nil || !s.isTrusted(peer) {
		return host
	}
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if addr, aerr := netip.ParseAddr(v); aerr == nil {
			return addr.String()
		}
	}
	return host
}

// isTrusted reports whether the direct peer is one of the configured hops.
func (s *Server) isTrusted(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	a := addr.Unmap()
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// sessionKeyFor decides which key a connection's durable subscription set lives
// under (D6).
//
// An AUTHENTICATED connection uses the token's own session claim — the
// refresh-token family the access token descends from — so the same person
// reconnecting, onto any replica, restores their channels. That is the
// affinity-free demo D6 describes, and it works because the client presents the
// same token again and the key is derived rather than remembered.
//
// An ANONYMOUS connection gets a FRESH RANDOM KEY. Its presence and its
// subscription set are recorded, so the fleet view covers every connection
// rather than only the authenticated minority, but nothing resumes: the key
// dies with the socket.
//
// That asymmetry is deliberate and is a gap rather than a design. Anonymous
// resume would require the client to present its previous session id on
// reconnect, and D8's client frames have nowhere to put one — the alternatives
// are a query parameter (which is where D5 spent an entire argument saying
// credentials do not go, and a session id that restores a watchlist is close
// enough to one) or a third subprotocol offer, which is a protocol addition D8
// does not make. It is left undone rather than done in the URL.
func sessionKeyFor(identity Identity, newID func() string) string {
	if identity.SessionID != "" {
		return identity.SessionID
	}
	return newID()
}
