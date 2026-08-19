// Command stream is the WebSocket gateway: subscription routing, the
// snapshot-then-delta protocol and per-client backpressure (CLAUDE.md §3, §5).
// It reads the compacted price.computed topic and turns it into a live board in
// a browser.
//
// This file is the COMPOSITION ROOT and nothing else. Every policy it appears to
// decide is argued somewhere else and imported here: the fanout, the routing
// table and the sequence discipline in internal/wsgw, the groupless bus read in
// internal/platform/kafka's Follower, the durable subscription set in
// internal/wsgw/redispresence, the token format in internal/auth. What this file
// owns is which of those objects exist, in what order they are built, what
// happens when one of them cannot be, and — the one thing that is genuinely
// only decidable here — the ORDER in which they stop.
//
// It is deliberately the sibling of cmd/api/main.go and cmd/pricer/main.go
// rather than a third idiom: the same probe branch as the first statement in
// main, the same registry-before-collectors ordering, the same bus
// ClientOptions shared by every client in the process, the same deferred-close
// discipline, and the same listener-and-pipeline-run-together / stop-together
// shutdown collecting into errors.Join.
//
// The one place it MUST diverge from those two is the shutdown sequence, and
// the divergence is D11 rather than taste: this service holds long-lived
// sockets, so "cancel one context and let everything unwind" would tear the bus
// follower down at the same instant the clients are being told to go away.
// See the shutdown sequencer at the bottom of run.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/kafka"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/redis"
	"github.com/anpl1623/sharpline/internal/wsgw"
	"github.com/anpl1623/sharpline/internal/wsgw/redispresence"
)

const service = "stream"

// shutdownBudget is the WHOLE going-away sequence: send every live connection a
// close frame, wait for the send queues to flush, close each socket, and wait
// for each connection's goroutines to finish (D11).
//
// It is an OUTER bound on wsgw.Options.ShutdownDrain (5s by default), not a
// second copy of it. The gateway's drain covers the flush and ends the moment
// every queue is empty; this covers the flush plus the per-connection close
// waits that follow it, so it must be strictly larger or the budget would
// expire on a step the gateway had not started yet.
//
// 10 seconds against the 20s deploy/compose gives this service
// (stop_grace_period: 20s) and the 30s Kubernetes default
// terminationGracePeriodSeconds. The remainder is not slack: the operational
// listener drains concurrently on httpx.DefaultShutdownTimeout, the bus
// follower stops after this returns, and a process that overruns the grace
// period is SIGKILLed — which resets every socket instead of closing it, and a
// reset is exactly the outcome the drain exists to avoid.
const shutdownBudget = 10 * time.Second

func main() {
	// `sharpline healthcheck` self-probes this service's own /readyz and exits 0
	// or 1. The runtime image is gcr.io/distroless/static:nonroot — no shell, no
	// wget, no curl — so this binary is the only executable a Docker healthcheck
	// or a Kubernetes exec probe can invoke. It must stay the FIRST statement in
	// main: everything below it opens sockets. See internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.Stream.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		// Config may have failed to load, so this is the only place a logger is
		// built without it. Exit non-zero so the orchestrator notices.
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.Stream)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// The build identity goes on the FIRST line this process writes, beside the
	// config; see cmd/api/main.go for why. It matters more here than anywhere
	// else in the tree: this is the service scaled by a custom-metric HPA on
	// active WebSocket connections (CLAUDE.md §9), so its replica set is the
	// most volatile in the cluster and `count by (version)` over
	// sharpline_build_info is how a rolling deploy of it is watched at all.
	//
	// Logging the whole Config cannot leak the Redis password: Config
	// implements slog.LogValuer and reports every secret as a boolean.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// SIGTERM is what Kubernetes and `docker stop` send; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The registry is built BEFORE anything that exports a series, which is the
	// only ordering that gets all three properties at once: every collector
	// lands on the same /metrics as the Go runtime collectors httpx.NewRegistry
	// installs, the dependencies exist before httpx.NewServer so they can be
	// passed as Checkers by value rather than through a mutable indirection, and
	// no global registry is touched (CLAUDE.md §12: no global mutable state).
	registry := httpx.NewRegistry()

	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// The gateway's collector set, including the three SHARED contract series
	// it adopts rather than redeclares — sharpline_odds_staleness_seconds,
	// sharpline_pipeline_latency_seconds and sharpline_odds_clock_skew_total.
	// internal/wsgw/metrics.go carries the argument; what matters here is that
	// it is registered on the same registry as everything else, because
	// stage="fanout" on the first of those IS the headline SLO (CLAUDE.md §9)
	// and the recording rules that read it have returned "No data" since phase
	// 0 waiting for this process to scrape.
	gwMetrics, err := wsgw.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	replica := replicaID(log)

	verifier, err := newVerifier(cfg, log)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// Normalised HERE rather than inside NewHub alone, because two of the
	// numbers it settles are needed by a collaborator built before the hub is:
	// redispresence must be given the SAME subscription TTL and the SAME
	// channel cap the gateway believes it has, or the durable set expires (or
	// refuses a channel) on a schedule nothing in internal/wsgw agreed to.
	// Normalise is idempotent, so passing the result to NewHub changes nothing.
	gwOpts := wsgw.Options{
		Logger:    log,
		Metrics:   gwMetrics,
		Verifier:  verifier,
		ReplicaID: replica,
		// Every other knob is left at internal/wsgw's documented defaults. They
		// are argued in options.go against the proxy layer, the fanout cost and
		// the CLAUDE.md §10 target, and a second copy of any of them here would
		// be a number a reviewer has to reconcile with the one that carries the
		// reasoning.
	}.Normalise()

	// ---- dependencies -------------------------------------------------------
	//
	// config.Stream declares RequireHTTP | RequireRedis | RequireKafka, and
	// phase 2's rule is that a declaration OBLIGES the binary to open the thing:
	// api and settle once declared Postgres without opening a pool and
	// /api/readyz answered 200 with the database stopped, a probe worse than
	// none. Both are opened here and both appear in Checkers below.

	// One collector set per process. There is only one bus client in this
	// binary today, but registering the set here rather than letting the client
	// build its own is what keeps sharpline_kafka_* a single series per topic if
	// a second one is ever added — the same shape cmd/pricer uses for three.
	busMetrics, err := kafka.NewMetrics(registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	bus := kafka.ClientOptions{
		Brokers: cfg.KafkaBrokers,
		Service: service,
		Logger:  log,
		Metrics: busMetrics,
	}

	// D1: a FOLLOWER, not a Consumer and not a Snapshotter.
	//
	// Direct partition assignment, no consumer group, no committed offsets,
	// reset to the start of the log and running on into the live tail without a
	// seam. A consumer group would split the partitions between replicas, so
	// with two `stream` pods each would hold half the slate and a client would
	// subscribe successfully, receive an empty snapshot and then receive
	// nothing, for ever, with nothing failing anywhere — which is precisely
	// what CLAUDE.md §9's affinity-free routing forbids. See ADR 0008.
	//
	// ErrorPolicySkip applies only AFTER catch-up; internal/platform/kafka's
	// Follower ignores the policy during the replay and treats every failure as
	// fatal, because "a snapshot with a hole in it is not a snapshot". Once the
	// slate is known complete, skipping is the right call for the same reason
	// cmd/pricer gives: halting the follower would freeze the whole board for
	// every client on this replica to preserve one market, and the next
	// price.computed record for that key replaces it anyway.
	follower, err := kafka.NewFollower(ctx, kafka.FollowerOptions{
		ClientOptions: bus,
		Topic:         kafka.TopicPriceComputed,
		ErrorPolicy:   kafka.ErrorPolicySkip,
		// CatchUpTimeout is left at its default. It is the number a Kubernetes
		// startupProbe has to be sized against — it bounds how long a replica
		// may take to hold the full slate before it is declared broken rather
		// than slow — and it belongs with the constant that documents it.
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := follower.Close(); err != nil {
			log.Error("closing the price follower failed", slog.String("error", err.Error()))
		}
	}()

	// Redis earns its place in this service for what CLAUDE.md §9 assigns it:
	// "subscription state lives in Redis rather than in a pod", which is what
	// makes the sticky-free load balancing in deploy/proxy/Caddyfile observable
	// rather than theoretical. It is never the source of truth (CLAUDE.md §3):
	// the CONTENT of every snapshot comes from the compacted Kafka topic above,
	// and Redis is consulted only for WHICH channels a returning session held.
	//
	// Note what redis.Connect does and what it therefore means: it awaits
	// readiness, so this process REFUSES TO START without Redis. D6's
	// "unreachable at connect, the connection is still served" is about a
	// CLIENT connecting to a running gateway whose Redis has gone away, not
	// about process startup — compose gates this service on
	// `redis: condition: service_healthy` and config.Stream declares the
	// dependency, so a stream replica that came up without one would be
	// contradicting its own configuration.
	rdb, err := redis.Connect(ctx, redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		Service:  service,
		Logger:   log,
		Registry: registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer func() {
		if err := rdb.Close(); err != nil {
			log.Error("closing the redis client failed", slog.String("error", err.Error()))
		}
	}()

	presence, err := redispresence.New(redispresence.Options{
		Client:  rdb,
		Logger:  log,
		Replica: replica,
		// The two numbers that MUST agree with the gateway, and the reason
		// gwOpts is normalised above rather than below.
		//
		// THE TWO PACKAGES DEFAULT TO DIFFERENT TTLs AND THIS FILE IS THE ONLY
		// PLACE THAT CAN CHOOSE. redispresence.DefaultTTL is 90s, argued
		// against the ping interval — "four and a half missed heartbeats", so a
		// SIGKILLed replica's presence keys are gone within a minute and a
		// half. wsgw.DefaultSubscriptionTTL is 5m, argued against the CLIENT —
		// it "has to exceed the reconnect window a client with a jittered
		// backoff actually uses (CLAUDE.md §7), otherwise a resume that the
		// design promises works on a fast reconnect and silently does not on a
		// slow one".
		//
		// The gateway's value wins, for a reason that is about this wiring
		// rather than about the two arguments: wsgw.Options.SubscriptionTTL is
		// the gateway's declared configuration for exactly this store, and if
		// this file did not pass it, that field would be a knob that does
		// nothing — a configuration value silently ignored is worse than either
		// number.
		//
		// The cost is named rather than hidden: a replica that is SIGKILLed
		// rather than drained leaves its ws:presence:{replica} set behind for
		// five minutes instead of ninety seconds, so the fleet view
		// over-reports connections for that long after a hard kill. Nothing
		// routes from that set — the routing table is in the pod (D6) — so the
		// cost is an operator's diagnosis, not a client's frames.
		//
		// A store cap BELOW the gateway's would be strictly worse than either
		// TTL choice: it would reject subscriptions the gateway had already
		// accepted and acknowledged, so they would work until the client
		// reconnected.
		TTL:         gwOpts.SubscriptionTTL,
		MaxChannels: gwOpts.MaxChannelsPerConnection,
		Registry:    registry,
	})
	if err != nil {
		return fmt.Errorf("%s: build the presence store: %w", service, err)
	}
	// Close marks the store closed so a late call after shutdown reports
	// ErrClosed instead of touching a client that is about to be closed. It
	// deliberately does NOT close rdb — that client is this file's to close, and
	// the defer above does it.
	defer func() { _ = presence.Close() }()

	// ---- the gateway --------------------------------------------------------

	hub, err := wsgw.NewHub(wsgw.HubOptions{
		Options:  gwOpts,
		Source:   follower,
		Presence: presence,
		// PresenceTimeout, Clock and NewConnectionID are left at their
		// defaults: the first is argued at wsgw.DefaultPresenceTimeout, and the
		// other two exist to make tests deterministic and have no production
		// value worth choosing.
	})
	if err != nil {
		return fmt.Errorf("%s: build the gateway hub: %w", service, err)
	}

	trusted, err := trustedProxies(log, cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	gateway, err := wsgw.NewServer(wsgw.ServerOptions{
		Hub: hub,
		// AllowedOrigins is EMPTY, which means same-origin only — and that is
		// the correct production setting rather than a placeholder. A WebSocket
		// is not protected by CORS: a page on any origin can open one to this
		// service and the browser will attach cookies, so the Origin check is
		// the only control there is. CLAUDE.md §7 puts the browser behind the
		// proxy ("never to a container hostname"), so the page and the socket
		// share an origin and nothing needs to be added. A deployment that
		// genuinely serves the frontend from another host adds patterns here;
		// there is deliberately no environment variable for it, because the one
		// value anybody would reach for under time pressure is "*", and
		// internal/wsgw refuses that outright.
		AllowedOrigins: nil,
		// MaxConnections is left at wsgw.DefaultMaxConnections, which is above
		// the CLAUDE.md §10 target of 10k on one node. The ceiling is not meant
		// to be reached; it converts an unbounded accept loop — which ends as an
		// OOM kill taking every connected client with it — into a bounded 503.
		TrustedProxies: trusted,
	})
	if err != nil {
		return fmt.Errorf("%s: build the upgrade handler: %w", service, err)
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// Readiness reflects exactly what this process opened.
		//
		//   *kafka.Follower  a real round trip to the broker on every probe,
		//                    reported under the name `kafka`.
		//   *redis.Client    likewise, under `redis`.
		//   *wsgw.Hub        under `stream`, and it is the one that matters:
		//                    it reports NOT READY until the compacted log has
		//                    been fully replayed. A replica that answered ready
		//                    with a partial slate would serve snapshots missing
		//                    markets, and nothing downstream could tell — an
		//                    empty snapshot is a legitimate answer for a channel
		//                    with no markets. There is no metric that catches
		//                    that; refusing to be routed traffic is the only
		//                    defence.
		//
		// Liveness deliberately consults none of them — a dependency outage must
		// not become a rolling restart of every replica at the moment the system
		// is least able to absorb one.
		//
		// AN HONEST NOTE ON REDIS BEING HERE. internal/wsgw's D6 says a Redis
		// outage costs resume-on-reconnect and nothing else, and listing rdb
		// means such an outage instead takes every replica out of rotation, so
		// no NEW client can connect (live sockets are untouched — a readiness
		// failure does not close them). The alternative — an httpx.CheckFunc
		// that reports Redis informationally and always returns nil — was
		// rejected: a probe that returns 200 for a dependency the binary
		// declared and opened is the phase-2 defect cmd/pricer describes, and
		// the right place to say "this gateway can run without Redis" is
		// config.Stream's Requires, not a probe that disagrees with it.
		Checkers: []httpx.Checker{follower, rdb, hub},
		// PublicPrefix is empty: the proxy forwards /ws here, not /stream/*, and
		// there is no public path under which mirroring the probes would make
		// sense. Prometheus scrapes :8081/metrics over the internal network.

		// ---- timeouts, and why each is what it is --------------------------
		//
		// ReadHeaderTimeout bounds the HANDSHAKE's header phase, which is the
		// actual slowloris surface and the only phase that is still an ordinary
		// HTTP request. It stays at the shared default so this listener is not
		// quietly the one service with a different slowloris posture.
		ReadHeaderTimeout: httpx.DefaultReadHeaderTimeout,
		// ReadTimeout and WriteTimeout are NEGATIVE, which httpx reads as "no
		// deadline". They are absolute deadlines on the whole request, set
		// before the connection is hijacked, and a live odds stream is ONE
		// request that stays open for hours — so any positive value here severs
		// it mid-stream. httpx/health.go names `stream` as the service that
		// needs this, and deploy/proxy/Caddyfile makes the identical choice one
		// hop in front (`read_body 0`, `write 0`) for the identical reason.
		// The per-write budget a stalled TCP peer actually needs is
		// wsgw.Options.WriteTimeout, applied by the connection loop around each
		// individual frame, which is a bound on a write rather than on a
		// lifetime.
		ReadTimeout:  -1,
		WriteTimeout: -1,
		// IdleTimeout must be stated EXPLICITLY and non-zero. It governs only
		// keep-alive connections BETWEEN requests, and a hijacked WebSocket has
		// left net/http's bookkeeping entirely, so it can never reap a stream.
		// The reason it cannot be left at zero is Go's fallback rule: a zero
		// IdleTimeout means "use ReadTimeout", and ReadTimeout here is "no
		// deadline" — so zero would leave an idle keep-alive connection open for
		// ever. The shared default is the right number for the traffic that
		// reaches this listener without upgrading: probes, scrapes, and the 400
		// or 503 refusals in front of the upgrade.
		IdleTimeout: httpx.DefaultIdleTimeout,
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	// The one application route. EXACT match, not a subtree.
	//
	// deploy/proxy/Caddyfile Route 1 matches `path /ws /ws/*` and forwards
	// WITHOUT stripping the prefix, so `/ws` arrives here as `/ws` and the
	// pattern matches what the proxy actually sends. `/ws/anything` is also
	// forwarded and 404s here, deliberately: there is exactly one gateway
	// endpoint, and a subtree pattern would upgrade a typo'd path and make a
	// client that is talking to nothing look like a client that is connected.
	srv.Handle("GET "+wsgw.Route, gateway)

	// The route table at startup. The runtime image is distroless and has no
	// shell, so there is no way to ask a running container what it serves; this
	// log line is the only place the answer appears. cmd/api does the same.
	log.Info("stream routes mounted",
		slog.String("websocket", "GET "+wsgw.Route),
		slog.String("protocol", wsgw.Protocol),
		slog.String("consumes", kafka.TopicPriceComputed),
		slog.String("replica", replica),
		slog.Bool("authenticated_subscriptions", verifier != nil),
	)

	// ---- run, and stop in the one order that is correct ---------------------
	//
	// D11: on SIGTERM, stop accepting upgrades, tell every live connection to go
	// away, drain, and only THEN stop the bus follower. That ordering is the
	// reason this is not cmd/pricer's two-goroutine shape verbatim.
	//
	// The follower runs on busCtx, which is deliberately DETACHED from the
	// signal context (context.WithoutCancel keeps its values — the trace context
	// among them — and drops its cancellation). If the follower shared ctx it
	// would stop at the same instant the drain began, and every client would be
	// told to go away by a replica that had already stopped knowing what the
	// prices were. Cancelling it after the drain instead costs a few seconds of
	// bus traffic nobody reads, which is the cheaper of the two.
	busCtx, stopBus := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBus()

	// listenerDone exists so an EARLY listener failure — the address already in
	// use, most likely — also triggers the sequencer. Without it the process
	// would sit holding a bus follower and no listener until somebody sent it a
	// signal, which is a hang that looks like a working service.
	listenerDone := make(chan struct{})

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	fail := func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		errs = append(errs, err)
		mu.Unlock()
	}

	wg.Add(3)
	go func() {
		defer wg.Done()
		defer close(listenerDone)
		fail(srv.Run(ctx))
	}()
	go func() {
		defer wg.Done()
		fail(hub.Run(busCtx))
	}()
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-listenerDone:
		}
		// Detached from the cancelled parent so the drain gets its full budget;
		// the same move httpx.Server.Run makes for its own shutdown, for the
		// same reason — a shutdown that inherits the cancellation that caused it
		// has no time to do anything.
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownBudget)
		defer cancel()
		fail(hub.Shutdown(drainCtx))
		stopBus()
	}()
	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log.Info("stopped")
	return nil
}

// newVerifier adapts internal/auth's token verifier to internal/wsgw's
// one-method seam, and decides what an absent signing key means.
//
// # There is exactly ONE verifier in this process, and it is the same one cmd/api uses
//
// internal/wsgw does not verify tokens and does not import internal/auth; it
// declares [wsgw.TokenVerifier] and takes this injection, exactly as
// internal/httpapi/middleware declares Authenticator and takes cmd/api's. Both
// defer to auth.TokenIssuer, which pins the signing algorithm from
// configuration and IGNORES the `alg` header on the presented token — so
// `alg: none` and an algorithm-confusion downgrade are unrepresentable rather
// than merely rejected — and which verifies issuer, audience and expiry. A
// second verifier anywhere in the tree would be a second place for that to be
// subtly wrong, and the second one is always the one nobody reviews.
//
// The raw token is passed to Verify wrapped in auth.Secret and is not returned,
// logged or attached to the identity: wsgw.Identity has no token field,
// deliberately.
//
// The audience is auth.DefaultAudience ("sharpline-api"), because that is what
// the API actually mints today. A distinct "sharpline-stream" audience is the
// better end state — it would stop a token scoped to the gateway from placing a
// wager — but it requires the API to mint a second token, which is a phase-5
// surface change deliberately not made here.
//
// # An absent signing key
//
// config.Stream deliberately does NOT declare RequireJWT, and internal/platform/
// config READS SHARPLINE_JWT_SIGNING_KEY anyway — the requirement and the use
// are different questions and that package now keeps them apart. So the key is
// optional here and an empty one is a legal, running configuration.
//
// It has to be optional. RequireJWT is a hard gate: the process refuses to
// start. Declaring it would mean a missing key takes down the PUBLIC odds
// board, and the board is public (CLAUDE.md §6) — an anonymous connection is a
// first-class state here, not a degraded one. What an absent key actually costs
// is authenticated subscriptions and, with them, D6's resume-on-reconnect keyed
// by the token's session claim; the whole read surface is unaffected.
//
// What must NOT happen in that state is the tempting one-liner: accept the
// token unverified, or ignore it and continue anonymously. Market data is
// public (CLAUDE.md §6), so an anonymous connection is a first-class state and
// the gateway keeps serving every board, every event and every market — but a
// client that PRESENTS a credential is refused, because a client that believes
// it is authenticated and is quietly not is the failure
// internal/httpapi/middleware's credentialMalformed branch exists to prevent.
// Returning a nil verifier is what produces that refusal: internal/wsgw's
// Authenticate turns "a credential, and no verifier" into ErrInvalidCredential
// and closes the socket.
//
// The WARN is loud and names what is missing, because a degraded control that
// says nothing is one nobody fixes — the same judgement cmd/api makes for an
// absent TOTP keyring and an empty trusted-proxy set.
func newVerifier(cfg *config.Config, log *slog.Logger) (wsgw.TokenVerifier, error) {
	if cfg.JWTSigningKey == "" {
		log.Warn("no JWT signing key is configured: authenticated subscriptions are unavailable",
			slog.String("effect", "anonymous connections are served in full (market data is "+
				"public); a connection that PRESENTS a token is refused, never downgraded"),
			slog.String("also_lost", "resume-on-reconnect keyed by the token's session claim, "+
				"so a returning client re-lists its channels instead of having them restored"),
			slog.String("needs", config.EnvJWTSigningKey+", the SAME key `api` mints with; "+
				"a different one would reject every token that service issued"),
			slog.String("why_not_accept_unverified", "a client that believes it is "+
				"authenticated and is quietly not is worse than one that is told no"),
		)
		return nil, nil
	}

	issuer, err := auth.NewTokenIssuer(auth.TokenIssuerOptions{
		SigningKey: []byte(cfg.JWTSigningKey),
	})
	if err != nil {
		return nil, fmt.Errorf("build the access-token verifier: %w", err)
	}

	return wsgw.TokenVerifierFunc(func(_ context.Context, token string) (wsgw.Identity, error) {
		claims, err := issuer.Verify(auth.NewSecret(token))
		if err != nil {
			// Returned VERBATIM. internal/auth's verification errors are
			// deliberately incurious — they say a token failed, never which
			// check it failed or whose token it was — and internal/wsgw turns
			// any of them into the same close frame. Adding context here is the
			// one way this adapter could reintroduce an oracle.
			return wsgw.Identity{}, err
		}
		return wsgw.Identity{
			UserID:    claims.Subject,
			SessionID: claims.SessionID,
			ExpiresAt: claims.ExpiresAt,
			// Nothing else is carried. wsgw.Identity has no field for an email,
			// a display name or an account status, and inventing one from a
			// claim would let this gateway act on a snapshot taken at mint time
			// — a customer who self-excludes at 14:00 must not be carried as
			// active until their token expires.
		}, nil
	}), nil
}

// replicaID is this pod's identity in the Redis presence set and in every log
// line this process writes about the fleet (D6).
//
// The hostname is the right source because BOTH deployment targets already set
// it to something unique and meaningful: Docker sets a container's hostname to
// its container id (deploy/compose declares no `hostname` and no
// `container_name` for this service, which is also what lets
// `docker compose up --scale stream=2` work at all), and Kubernetes sets it to
// the pod name. So `ws:presence:{replica}` in redis-cli, the `replica` field in
// a log line, and `kubectl get pods` all name the same thing without anything
// being configured.
//
// # Why an unusable hostname falls back instead of failing startup
//
// redispresence.New REFUSES a Replica that is not a safe Redis key segment
// rather than sanitising it, so that the key an operator greps and the string a
// log line prints are identical. That is the right call there. It would be the
// wrong call here: a developer whose machine is named "Andrew's Mac" would get a
// gateway that will not start, for a reason that has nothing to do with serving
// odds. A generated id costs the operator the ability to map a presence key back
// to a host — which is why the fallback is LOGGED at WARN with the value that
// was rejected — and costs the service nothing.
//
// The segment rule is restated here rather than imported because
// redispresence's own check is unexported. That duplication is acceptable in
// exactly one direction: this check is a PRE-FILTER whose only effect is to
// choose the fallback earlier, and redispresence.New remains the authority. If
// the two ever disagree, New wins and the process refuses to start, which is
// visible rather than silent.
func replicaID(log *slog.Logger) string {
	host, err := os.Hostname()
	switch {
	case err != nil:
		log.Warn("could not read the hostname; using a generated replica id",
			slog.String("error", err.Error()),
			slog.String("effect", "fleet presence keys cannot be mapped back to a host"))
	case safeKeySegment(host):
		return host
	default:
		log.Warn("the hostname is not usable as a Redis key segment; using a generated replica id",
			slog.String("hostname", host),
			slog.String("rule", "[A-Za-z0-9._-]{1,96}"),
			slog.String("effect", "fleet presence keys cannot be mapped back to a host"))
	}

	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read does not fail on any platform this ships to, and
		// CLAUDE.md §12 forbids a panic outside main — but an identity that is
		// silently the same on every replica would merge two pods' presence
		// sets, which is worse than refusing to start. Returning an empty
		// string does refuse: redispresence.New rejects it by name.
		log.Error("could not generate a replica id", slog.String("error", err.Error()))
		return ""
	}
	return "stream-" + hex.EncodeToString(b[:])
}

// safeKeySegment mirrors redispresence's own key-segment rule. See [replicaID]
// for why this copy exists and why it is safe.
func safeKeySegment(s string) bool {
	const maxLen = 96
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// trustedProxies is the set of peers whose X-Real-IP this gateway believes.
//
// # Why the parse is imported rather than rewritten
//
// internal/httpapi/middleware.ParseTrustedProxies is exported and is already the
// one implementation cmd/api uses, so importing it means SHARPLINE_TRUSTED_PROXIES
// has exactly one meaning across the two services that read it. internal/wsgw's
// own comment at Server.clientAddr says this in the general case: "the correct
// move is to export the existing implementation and use it, rather than to grow
// a second copy of a security-sensitive parse."
//
// # Why an empty set is a much smaller problem here than it is for the API
//
// For cmd/api an empty set is a degraded CONTROL: every request buckets on the
// proxy's own address, so one abusive client consumes the per-IP rate limit for
// everyone behind it. This gateway rate-limits nothing by address. The value is
// a LOG FIELD and nothing in internal/wsgw makes a decision from it, which is
// stated at Server.clientAddr and is the precondition for that function
// implementing only the X-Real-IP half of the discipline.
//
// So this logs at INFO rather than WARN, and says plainly what is lost: with no
// trusted set, every connection in this service's logs is attributed to the
// proxy's bridge address, and correlating a client's socket with its REST
// requests stops working. If this gateway ever limits by address, that stops
// being a logging inconvenience and this comment stops being true.
func trustedProxies(log *slog.Logger, cfg *config.Config) ([]netip.Prefix, error) {
	if len(cfg.TrustedProxies) == 0 {
		log.Info("no trusted proxy set is configured",
			slog.String("effect", "every connection is attributed to the reverse proxy's own "+
				"address in this service's logs"),
			slog.String("needs", config.EnvTrustedProxies+
				", set to the compose bridge subnet or the ingress pod CIDR"),
			slog.String("not_affected", "nothing in the gateway makes a decision from the "+
				"client address; it is a log field"),
		)
		return nil, nil
	}

	trusted, err := middleware.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", config.EnvTrustedProxies, err)
	}
	log.Info("trusted proxy set configured",
		slog.Any("cidrs", cfg.TrustedProxies),
		slog.String("effect", "X-Real-IP is believed from these peers and from nobody else"),
	)
	return trusted, nil
}

// compile-time assertions that the seams this file depends on have not moved.
var (
	_ httpx.Checker      = (*kafka.Follower)(nil)
	_ httpx.Checker      = (*redis.Client)(nil)
	_ httpx.Checker      = (*wsgw.Hub)(nil)
	_ wsgw.TokenVerifier = (wsgw.TokenVerifierFunc)(nil)
)
