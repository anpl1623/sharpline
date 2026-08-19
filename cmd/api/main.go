// Command api serves the Sharpline REST API: auth, catalogue, account, bet
// slip, wagers and history (CLAUDE.md §3). Phase 5 delivers auth, the catalogue
// read surface and account; the bet slip and wagers arrive in phase 8 as
// additional route sets and change nothing about the shape of this file.
//
// This file is the COMPOSITION ROOT and nothing else. Every policy it appears
// to decide is argued somewhere else and imported here: the middleware order in
// internal/httpapi/middleware, the route table in internal/httpapi, the token
// format in internal/auth, the pool geometry in internal/platform/postgres. What
// this file owns is which of those objects exist, in what order they are built,
// and what happens when one of them cannot be.
//
// It is deliberately the sibling of cmd/ingest/main.go and cmd/pricer/main.go
// rather than a third idiom: the same probe branch as the first statement in
// main, the same registry-before-collectors ordering, the same
// deferred-close discipline, and the same "the listener owns the shutdown"
// shape.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/anpl1623/sharpline/internal/auth"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
	"github.com/anpl1623/sharpline/internal/httpapi/pgstore"
	"github.com/anpl1623/sharpline/internal/platform/buildinfo"
	"github.com/anpl1623/sharpline/internal/platform/config"
	"github.com/anpl1623/sharpline/internal/platform/httpx"
	"github.com/anpl1623/sharpline/internal/platform/logging"
	"github.com/anpl1623/sharpline/internal/platform/postgres"
	"github.com/anpl1623/sharpline/internal/platform/redis"
)

const service = "api"

// publicPrefix is the path prefix the reverse proxy forwards to this service
// verbatim (deploy/proxy/Caddyfile, Route 2: "the prefix is PRESERVED, not
// stripped"). openapi.yaml says the same thing from the other side with
// `servers: - url: /api/v1`.
const publicPrefix = httpapi.DefaultPublicPrefix

// Rate-limit shapes for the two controls CLAUDE.md §6 requires: "rate limiting
// per user and per IP".
//
// # Why these live here and not in internal/httpapi/middleware
//
// The middleware owns the MECHANISM — a Redis token bucket, so a limit holds
// across every replica rather than being multiplied by the replica count
// (CLAUDE.md §9 runs api behind an Ingress with more than one). The NUMBERS are
// policy, and policy belongs to the deployment. They are constants here rather
// than literals at the call site so they are one edit, and they are named so
// the reasoning is attached to them.
//
// # Why the per-IP limit is the looser of the two
//
// An IP is not a user. A university, an office or a mobile carrier NATs
// thousands of people behind one address, so a per-IP limit tight enough to be
// interesting for one person is an outage for all of them. Its job is to bound
// an anonymous flood — the scanner walking paths that do not exist, the
// credential-stuffing run against /auth/login — not to be the primary control.
// The per-user limit is the one that shapes a real client's behaviour, and it
// can be tighter precisely because it names an account rather than a household.
//
// Burst is a full window in both cases: a browser opening the odds board issues
// a page's worth of requests at once and then goes quiet, and a limiter that
// smoothed that into a trickle would make the first paint slow to punish a
// pattern that is not abusive.
var (
	ipRateLimit   = redis.Limit{Requests: 600, Window: time.Minute, Burst: 600}
	userRateLimit = redis.Limit{Requests: 300, Window: time.Minute, Burst: 300}
)

func main() {
	// `sharpline healthcheck` self-probes this service's own /readyz and exits 0
	// or 1. The runtime image is gcr.io/distroless/static:nonroot — no shell, no
	// wget, no curl — so this binary is the only executable a Docker healthcheck
	// or a Kubernetes exec probe can invoke. It must stay the FIRST statement in
	// main: everything below it opens sockets. See internal/platform/httpx/probe.go.
	if httpx.IsProbeInvocation(os.Args) {
		os.Exit(httpx.Probe(context.Background(),
			config.EnvHTTPAddr, config.API.DefaultHTTPAddr, httpx.PathReadyz, os.Stderr))
	}

	if err := run(); err != nil {
		// Config may have failed to load, so this is the only place a logger is
		// built without it. Exit non-zero so the orchestrator notices.
		logging.Bootstrap(os.Stderr, service).Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.API)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log := logging.New(os.Stdout, cfg.LogLevel, service, cfg.Env)
	// The build identity goes on the FIRST line this process writes, beside the
	// config, because it answers the question every other line depends on: which
	// code produced them. The runtime image is distroless — no shell — so there
	// is no `sharpline --version` an operator can run against a container to
	// find out afterwards.
	//
	// Logging the whole Config cannot leak the JWT signing key or the Redis
	// password: Config implements slog.LogValuer and reports every secret as a
	// boolean. That is a property of the config package, not of this call site
	// remembering to be careful.
	log.Info("starting",
		slog.Any("build", buildinfo.Read()),
		slog.Any("config", cfg),
	)

	// SIGTERM is what Kubernetes and `docker stop` send; SIGINT is Ctrl-C.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// The registry is built BEFORE anything that exports a series, which is the
	// only ordering that gets all three properties at once: every collector lands
	// on the same /metrics as the Go runtime collectors httpx.NewRegistry
	// installs, the dependencies exist before httpx.NewServer so they can be
	// passed as Checkers by value rather than through a mutable indirection, and
	// no global registry is touched (CLAUDE.md §12: no global mutable state).
	registry := httpx.NewRegistry()

	// sharpline_build_info: the same three facts as the log line above, as labels
	// on a constant 1, so "which build is this replica on" is answerable from
	// Prometheus during a rolling deploy rather than only from whichever log
	// lines are still in the retention window.
	if err := buildinfo.Register(registry, service); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	// ---- dependencies -------------------------------------------------------
	//
	// config.API declares RequirePostgres and RequireRedis, and phase 2's rule is
	// that a declaration OBLIGES the binary to open the thing: api and settle
	// once declared Postgres without opening a pool, and /api/readyz answered 200
	// with the database stopped — a probe worse than none. Both are opened here
	// and both appear in Checkers below, so /readyz makes a real round trip
	// through each on every probe.

	db, err := postgres.Connect(ctx, postgres.Options{
		DSN:      cfg.PostgresDSN,
		Service:  service,
		Logger:   log,
		Registry: registry,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}
	defer db.Close()

	// Redis earns its place in this service for what CLAUDE.md §3 assigns it and
	// the API owns: DISTRIBUTED RATE LIMITING. §6 requires it per user and per
	// IP, and §9 runs api behind an Ingress with multiple replicas, so an
	// in-process limiter would silently multiply every limit by the replica
	// count. It is never the source of truth: every value this API returns comes
	// from Postgres, and Redis holds only counters a cold replica reconstructs by
	// doing nothing.
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

	// ---- the middleware chain ----------------------------------------------

	ipLimiter, err := redis.NewRateLimiter(redis.RateLimiterOptions{
		Client: rdb,
		Scope:  "ip",
		Limit:  ipRateLimit,
	})
	if err != nil {
		return fmt.Errorf("%s: build the per-IP rate limiter: %w", service, err)
	}

	userLimiter, err := redis.NewRateLimiter(redis.RateLimiterOptions{
		Client: rdb,
		Scope:  "user",
		Limit:  userRateLimit,
	})
	if err != nil {
		return fmt.Errorf("%s: build the per-user rate limiter: %w", service, err)
	}

	authenticator, err := newAuthenticator(cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	trusted, err := trustedProxies(log, cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	stack, err := middleware.NewStack(middleware.StackOptions{
		Service:  service,
		Logger:   log,
		Registry: registry,
		// RouteFunc is nil ON PURPOSE. It is the fast path for a chain wrapping
		// an *http.ServeMux directly; here the router is httpapi.Server, which
		// owns its own mux and calls middleware.SetRoute from inside its
		// dispatch. ResolveRoute still installs the cell, so the pattern reaches
		// the metric labels, the span name and the access log line exactly as it
		// would have — and passing a mux this file does not own would be a
		// second router that could disagree with the real one.
		RouteFunc:      nil,
		TrustedProxies: trusted,
		IPLimiter:      ipLimiter,
		UserLimiter:    userLimiter,
		Authenticator:  authenticator,
		// The service's own envelope, so a 429 from the limiter, a 401 from the
		// authenticator and a 422 from a handler are the same four keys on the
		// wire. internal/httpapi/middleware declines to own the schema and takes
		// this injection precisely so there is one shape for pkg/client to
		// decode; httpapi.WriteAPIError renders gen.Error, generated FROM
		// openapi.yaml, so it cannot drift from the contract.
		ErrorWriter: httpapi.WriteAPIError,
		// CORS is left at its zero value, which denies every cross-origin
		// request. CLAUDE.md §7: "The browser talks to the API through the
		// reverse proxy, never to a container hostname" — same origin, so there
		// is no preflight to answer. A permissive policy added "so the frontend
		// works" is one of the few ways an authenticated JSON API is most
		// commonly given away.
	})
	if err != nil {
		return fmt.Errorf("%s: build the middleware chain: %w", service, err)
	}

	// ---- the public HTTP surface -------------------------------------------

	store, err := pgstore.New(pgstore.Options{DB: db})
	if err != nil {
		return fmt.Errorf("%s: build the postgres adapter: %w", service, err)
	}

	routeSets, err := routeSets(log, stack, store)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	api, err := httpapi.New(httpapi.Options{
		PublicPrefix: publicPrefix,
		RouteSets:    routeSets,
		Middleware:   stack.Middlewares(),
		ErrorWriter:  stack.ErrorWriter(),
	})
	if err != nil {
		return fmt.Errorf("%s: build the api router: %w", service, err)
	}

	srv, err := httpx.NewServer(httpx.ServerOptions{
		Service:  service,
		Addr:     cfg.HTTPAddr,
		Logger:   log,
		Registry: registry,
		// Readiness reflects exactly what this process opened. *postgres.DB and
		// *redis.Client both satisfy httpx.Checker and both execute a real round
		// trip on every probe rather than latching a boolean at startup: a
		// readiness endpoint that stays green while a dependency is unreachable
		// is worse than none, because the orchestrator keeps routing to a replica
		// that cannot serve.
		//
		// Liveness deliberately consults neither — a dependency outage must not
		// become a rolling restart of every replica at the moment the system is
		// least able to absorb one (internal/platform/postgres/health.go).
		//
		// There is no Kafka checker because this binary opens no bus client.
		// config.API declares RequireKafka, which under the phase-2 rule would
		// oblige one; the API produces nothing to the bus until phase 8 writes
		// wager.events. Reported upward for reconciliation rather than satisfied
		// by inventing a producer nothing publishes to.
		Checkers: []httpx.Checker{db, rdb},
		// The proxy forwards /api/* here without stripping, so this mirrors
		// /healthz and /readyz beneath the prefix — and mirrors nothing else.
		// /metrics is deliberately NOT mirrored: the Caddyfile hard-denies
		// /metrics* at the site root and mirroring it here would punch straight
		// through that deny rule.
		PublicPrefix: publicPrefix,
	})
	if err != nil {
		return fmt.Errorf("%s: build operational listener: %w", service, err)
	}

	// One subtree pattern, registered on the operational listener's mux. The two
	// probes httpx already mirrored beneath the prefix are more specific patterns
	// and still win, so `GET /api/healthz` keeps answering from httpx.
	api.Mount(srv)

	// The route table at startup. The runtime image has no shell, so there is no
	// way to ask a running container what it serves; this is the only place the
	// answer appears.
	log.Info("api routes mounted",
		slog.String("prefix", api.Prefix()),
		slog.Int("routes", len(api.Patterns())),
		slog.Any("patterns", api.Patterns()),
	)

	// Run serves until ctx is cancelled, then stops accepting and drains
	// in-flight requests within httpx.DefaultShutdownTimeout. The deferred closes
	// above run afterwards, so the ordering on SIGTERM is: stop accepting →
	// drain → close Redis → close the pool → exit 0.
	if err := srv.Run(ctx); err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	log.Info("stopped")
	return nil
}

// newAuthenticator adapts internal/auth's token verifier to the middleware's
// one-method seam.
//
// internal/httpapi/middleware says of Authenticator that "a cmd/ entrypoint can
// adapt its verifier in one line", and this is that line plus its argument.
//
// # There is exactly ONE verifier in this process
//
// The middleware does not verify tokens and internal/httpapi does not verify
// tokens; both defer to auth.TokenIssuer, which pins the signing algorithm and
// IGNORES the `alg` header on the presented token — so `alg: none` and an
// algorithm-confusion downgrade are unrepresentable rather than merely
// rejected — and which verifies issuer, audience and expiry. A second verifier
// anywhere in the tree would be a second place for that to be subtly wrong.
//
// The raw token is passed to Verify wrapped in auth.Secret and is not returned,
// logged or attached to the identity: middleware.Identity has no token field,
// deliberately.
func newAuthenticator(cfg *config.Config) (middleware.Authenticator, error) {
	issuer, err := auth.NewTokenIssuer(auth.TokenIssuerOptions{
		SigningKey: []byte(cfg.JWTSigningKey),
	})
	if err != nil {
		return nil, fmt.Errorf("build the access-token verifier: %w", err)
	}

	return middleware.AuthenticatorFunc(func(_ context.Context, token string) (middleware.Identity, error) {
		claims, err := issuer.Verify(auth.NewSecret(token))
		if err != nil {
			// Returned verbatim: internal/auth's verification errors are
			// deliberately incurious — they say a token failed, never which
			// check it failed or whose token it was — and the middleware turns
			// any of them into the same 401. Adding context here is the one way
			// this adapter could reintroduce an oracle.
			return middleware.Identity{}, err
		}
		return middleware.Identity{
			UserID:    claims.Subject,
			SessionID: claims.SessionID,
			IssuedAt:  claims.IssuedAt,
			ExpiresAt: claims.ExpiresAt,
			// AMR is empty: the access token carries no `amr` claim today, and
			// inventing one here would let an endpoint believe a second factor
			// was used when nothing proved it. An endpoint that must insist on
			// TOTP re-proves it rather than trusting a claim this adapter made
			// up.
		}, nil
	}), nil
}

// trustedProxies is the set of peers whose X-Real-IP this service believes.
//
// # Why this returns an empty set and says so loudly
//
// Getting it wrong in either direction is a real failure. Trust the header from
// anyone and every client picks its own rate-limit bucket by sending it, so the
// per-IP control CLAUDE.md §6 requires stops existing. Trust nobody and every
// request buckets on the PROXY's address, so one abusive client consumes the
// limit for everyone behind it.
//
// The correct value is "exactly the hop in front of this service" — the compose
// bridge subnet the `proxy` container sits on, or the ingress controller's pod
// CIDR — and that is a deployment fact, not something this binary can know. A
// blanket private-range default would be the wrong answer with the right shape:
// on a shared bridge network "trust RFC1918" means "trust every container",
// which is every container that could be compromised.
//
// So it is empty until internal/platform/config grows SHARPLINE_TRUSTED_PROXIES,
// and this logs a warning naming the exact consequence rather than leaving a
// degraded control to be discovered from a graph. Reading the variable directly
// with os.Getenv would be a second configuration path, which CLAUDE.md §12
// forbids and which is how config drifts between the compose stack and the Helm
// chart.
func trustedProxies(log *slog.Logger, cfg *config.Config) (middleware.TrustedProxies, error) {
	_ = cfg
	log.Warn("no trusted proxy set is configured: per-IP rate limiting will bucket every "+
		"request on the reverse proxy's own address",
		slog.String("effect", "one client behind the proxy can exhaust the shared per-IP limit"),
		slog.String("needs", "SHARPLINE_TRUSTED_PROXIES in internal/platform/config, "+
			"set to the compose bridge subnet or the ingress pod CIDR"),
		slog.String("requirement", "CLAUDE.md §6: rate limiting per user and per IP"),
	)
	return nil, nil
}

// routeSets returns the API's route sets, in mount order.
//
// # The empty case is a decision, not an omission
//
// httpapi.NewAPI refuses to build without every port it needs, and the session
// port — registration, login, refresh-token rotation, TOTP enrolment — has no
// adapter in the tree yet: internal/auth.Service implements the behaviour but
// nothing adapts its method set to httpapi.Sessions. Rather than fabricate one,
// this returns no route sets and says why.
//
// What that produces is a service that starts, passes its probes, serves
// /metrics, and answers every path beneath /api with the spec's own 404
// envelope. CLAUDE.md §11 requires that `docker compose up` never fail, and an
// empty surface with a correct empty response is the honest way to honour that
// — a stub returning fabricated events would not be.
func routeSets(log *slog.Logger, stack *middleware.Stack, store *pgstore.Store) ([]httpapi.RouteSet, error) {
	sessions := sessionsPort()
	if sessions == nil {
		log.Warn("no session adapter is wired: the API is serving its operational surface only",
			slog.String("missing", "an httpapi.Sessions implementation over internal/auth.Service"),
			slog.String("effect", "every path beneath /api answers a spec-shaped 404; no data is fabricated"),
		)
		return nil, nil
	}

	api, err := httpapi.NewAPI(httpapi.APIOptions{
		Catalogue: store,
		Prices:    store,
		Ledger:    store,
		Accounts:  store,
		Limits:    store,
		Audit:     store,
		Sessions:  sessions,
		// Cache is nil: the Redis snapshot in front of Prices is an optimisation
		// and every read goes to Postgres without it, which is correct and
		// slower. It arrives with the market-state store rather than being
		// invented here.
		Cache: nil,
		// RequireAuth is REQUIRED by NewAPI and has no default, deliberately: a
		// default would make every account route public the moment a caller
		// forgot the field. The gate is bound to the stack's metrics and error
		// writer so a rejection is counted and rendered like every other.
		RequireAuth: []httpapi.Middleware{stack.RequireIdentity()},
		Logger:      log,
	})
	if err != nil {
		return nil, fmt.Errorf("build the api handlers: %w", err)
	}
	return []httpapi.RouteSet{api}, nil
}

// sessionsPort returns the adapter over internal/auth.Service, once one exists.
//
// It is a named function returning nil rather than a nil literal at the call
// site so that wiring it is a one-line change in one place, and so that the
// absence is greppable.
func sessionsPort() httpapi.Sessions { return nil }

// compile-time assertions that the seams this file depends on have not moved.
var (
	_ middleware.Limiter = (*redis.RateLimiter)(nil)
	_ httpx.Checker      = (*postgres.DB)(nil)
	_ httpx.Checker      = (*redis.Client)(nil)
	_ httpapi.Mux        = (*httpx.Server)(nil)
)
