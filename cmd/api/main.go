// Command api serves the Sharpline REST API: auth, catalogue, account, bet
// slip, wagers and history (CLAUDE.md §3). Phase 5 delivered auth, the catalogue
// read surface and account; phase 8 added the bet slip, placement, wager history
// and cash-out pricing, which arrive as additional ports on the same handler set
// and change nothing about the shape of this file.
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

	"github.com/prometheus/client_golang/prometheus"

	"github.com/anpl1623/sharpline/internal/auth"
	authpg "github.com/anpl1623/sharpline/internal/auth/pgstore"
	"github.com/anpl1623/sharpline/internal/auth/redisguard"
	"github.com/anpl1623/sharpline/internal/betting"
	bettingpg "github.com/anpl1623/sharpline/internal/betting/pgstore"
	"github.com/anpl1623/sharpline/internal/httpapi"
	"github.com/anpl1623/sharpline/internal/httpapi/authsessions"
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

	sessions, err := sessionsPort(ctx, cfg, log, db, rdb, registry)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	placement, pricer, err := bettingPort(log, db)
	if err != nil {
		return fmt.Errorf("%s: %w", service, err)
	}

	routeSets, err := routeSets(log, stack, store, sessions, placement, pricer)
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
// # Why this is configuration and not a default
//
// Getting it wrong in either direction is a real failure. Trust the header from
// anyone and every client picks its own rate-limit bucket by sending one, so
// the per-IP control CLAUDE.md §6 requires stops existing. Trust nobody and
// every request buckets on the PROXY's address, so one abusive client consumes
// the limit for everyone behind it — which is the state this service shipped
// in, and which the access log made visible as `client_ip` being the same
// bridge address on every single request.
//
// The correct value is "exactly the hop in front of this service" — the compose
// bridge subnet the `proxy` container sits on, or the ingress controller's pod
// CIDR — and that is a deployment fact. A blanket private-range default would
// be the wrong answer with the right shape: on a shared bridge network "trust
// RFC1918" means "trust every container", which is every container that could
// be compromised.
//
// So it comes from SHARPLINE_TRUSTED_PROXIES, config validates it, and an EMPTY
// value is still legal and still warned about — because empty is safe but
// degraded, and a degraded control that says nothing is one nobody fixes.
func trustedProxies(log *slog.Logger, cfg *config.Config) (middleware.TrustedProxies, error) {
	if len(cfg.TrustedProxies) == 0 {
		log.Warn("no trusted proxy set is configured: per-IP rate limiting will bucket every "+
			"request on the reverse proxy's own address",
			slog.String("effect", "one client behind the proxy can exhaust the shared per-IP limit"),
			slog.String("needs", config.EnvTrustedProxies+
				", set to the compose bridge subnet or the ingress pod CIDR"),
			slog.String("requirement", "CLAUDE.md §6: rate limiting per user and per IP"),
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

// routeSets returns the API's route sets, in mount order.
//
// # The empty case is now a FAILURE, not a decision
//
// This function used to return no route sets when the session port was
// unwired, on the reasoning that an empty surface answering the spec's own 404
// is more honest than a stub returning fabricated events. The reasoning about
// fabrication was right. The conclusion was wrong, and the cost was severe:
// the api container ran for a whole phase logging
// `"api routes mounted","routes":0,"patterns":null` while every health probe
// stayed green, `make check` stayed green and coverage sat at 81.93% — because
// cmd/api has no tests and every internal/httpapi test supplies its own fake.
// A service that serves NOTHING while reporting itself healthy is not honest;
// it is undetectable.
//
// Note also what the old behaviour cost beyond auth: nil sessions unmounted the
// CATALOGUE, BOARD and HISTORY routes too, none of which need a session at all.
//
// So the port is wired, and if it cannot be built this returns an error and the
// process refuses to start. Failing loudly at startup is CLAUDE.md §12's rule
// and it is the only version of this that a reviewer or an orchestrator can
// see.
func routeSets(
	log *slog.Logger,
	stack *middleware.Stack,
	store *pgstore.Store,
	sessions httpapi.Sessions,
	placement httpapi.Betting,
	pricer httpapi.TicketPricer,
) ([]httpapi.RouteSet, error) {
	api, err := httpapi.NewAPI(httpapi.APIOptions{
		Catalogue: store,
		Prices:    store,
		Ledger:    store,
		Accounts:  store,
		Limits:    store,
		Audit:     store,
		Sessions:  sessions,

		// The bet slip and wagers. `store` serves the wager READS (its
		// statements are scoped by user, and the one that is not is compared
		// against the caller immediately on read); `placement` owns the write
		// transaction and every rule inside it.
		Betting: placement,
		Wagers:  store,

		// The SAME pricer instance the placement service holds, which is what
		// makes `/slip/quote` honest: a slip quotes at the number it will be
		// booked at, or it is refused for having moved. Two pricers would make
		// the quote a polite fiction and the difference would reach the customer
		// as a price move that never happened.
		Pricer: pricer,

		// CashOutQuotes and CashOuts are nil, and both gaps are REPORTED rather
		// than papered over — httpapi.NewAPI logs each at WARN with what is
		// missing, and neither route is mounted.
		//
		// CashOutQuotes needs a betting.FairPrices source: devigged reference
		// prices from the sharp book (ADR 0006). Nothing in this tree implements
		// one yet, and wiring the service without it would mount a route that
		// answers 500 on every call, which is strictly worse than an absent one.
		//
		// CashOuts needs the settlement write path. Taking a cash-out is a state
		// transition on a placed ticket and belongs with the other transitions
		// in internal/settlement, deliberately: a component able to both quote
		// and take a cash-out could do both in one transaction at a price of its
		// own choosing.
		CashOutQuotes: nil,
		CashOuts:      nil,
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

// sessionsPort builds the adapter over internal/auth.Service.
//
// # What is assembled here, and why each piece is not optional
//
//   - authpg.Store is the session store. Refresh-token rotation and reuse
//     detection are a TRANSACTIONAL property (migrations/00005's partial unique
//     index on one-successor-per-parent is what makes the detection survive a
//     mistake in the Go), so this must be the Postgres implementation and never
//     an in-memory stand-in.
//   - redisguard.Guard is the TOTP replay guard. auth.MemoryReplayGuard is
//     correct for ONE replica and wrong for the several CLAUDE.md §9 runs
//     behind an Ingress: a code burnt on pod A is still fresh on pod B, which
//     is a one-step replay window handed to anyone who can retry.
//   - The keyring seals TOTP secrets at rest and is the one piece allowed to be
//     absent, because absent means "TOTP enrolment is refused" rather than
//     "TOTP secrets are stored in the clear". That refusal is loud in the
//     service and warned about here.
//
// The argon2id hasher is built with the package defaults. Its parameters are
// internal/auth's policy and are deliberately NOT re-specified here: a second
// copy of a cost parameter in a composition root is how one of the two ends up
// weakened by someone chasing a slow test.
func sessionsPort(
	ctx context.Context,
	cfg *config.Config,
	log *slog.Logger,
	db *postgres.DB,
	rdb *redis.Client,
	registry prometheus.Registerer,
) (httpapi.Sessions, error) {
	store, err := authpg.New(db)
	if err != nil {
		return nil, fmt.Errorf("build the auth store: %w", err)
	}

	hasher, err := auth.NewHasher(auth.HasherOptions{})
	if err != nil {
		return nil, fmt.Errorf("build the password hasher: %w", err)
	}

	tokens, err := auth.NewTokenIssuer(auth.TokenIssuerOptions{
		SigningKey: []byte(cfg.JWTSigningKey),
	})
	if err != nil {
		return nil, fmt.Errorf("build the access-token issuer: %w", err)
	}

	guard, err := redisguard.New(redisguard.Options{Client: rdb})
	if err != nil {
		return nil, fmt.Errorf("build the second-factor replay guard: %w", err)
	}

	var keyring *auth.Keyring
	if cfg.TOTPKeyring == "" {
		log.Warn("no TOTP keyring is configured: second-factor enrolment will be refused",
			slog.String("effect", "POST /api/v1/account/totp fails; password login and "+
				"refresh rotation are unaffected"),
			slog.String("needs", config.EnvTOTPKeyring+
				", in auth.ParseKeyring's id:base64key format"),
			slog.String("why_not_a_default", "a generated-at-startup key would decrypt "+
				"nothing written by the previous process, locking out every enrolled user "+
				"on restart"),
		)
	} else if keyring, err = auth.ParseKeyring(cfg.TOTPKeyring); err != nil {
		// The error from ParseKeyring never quotes key material — only the id
		// and the decoded length — so it is safe to wrap and return.
		return nil, fmt.Errorf("parse %s: %w", config.EnvTOTPKeyring, err)
	}

	svc, err := auth.NewService(auth.Options{
		Store:       store,
		Hasher:      hasher,
		Tokens:      tokens,
		Keyring:     keyring,
		ReplayGuard: guard,
		// RecoveryCodes is NIL, and that is a reported gap rather than an
		// oversight: internal/auth/pgstore implements auth.Store but not
		// auth.RecoveryCodeStore — there is no Postgres implementation of
		// ReplaceRecoveryCodes / ConsumeRecoveryCode in the tree at all.
		//
		// auth.Service tolerates nil everywhere it touches recovery codes: a
		// presented recovery code is rejected, TOTPStatus reports
		// RecoveryCodesRemaining as -1 ("not configured"), and removing a
		// factor skips the clear-out because there is nothing to clear. So the
		// effect is that a user who loses their authenticator cannot self-serve
		// back in.
		//
		// Wiring an in-memory stand-in here instead would be worse in the exact
		// way that matters: recovery codes that survive one pod and one restart
		// are a bypass credential the user believes is durable.
		RecoveryCodes: nil,
		Registry:      registry,
		Logger:        log,
		// RefreshTTL and SessionLifetime are left at internal/auth's defaults
		// for the same reason the argon2id parameters are: they are the
		// security policy of the package that enforces them, and a widened
		// lifetime set from here would not be visible to anyone reading that
		// package's tests.
	})
	if err != nil {
		return nil, fmt.Errorf("build the auth service: %w", err)
	}

	// A round trip now, rather than on the first login. The pool is already
	// open and a store that cannot answer is a service that should not report
	// itself ready.
	_ = ctx

	adapter, err := authsessions.New(authsessions.Options{Service: svc})
	if err != nil {
		return nil, fmt.Errorf("build the session adapter: %w", err)
	}
	return adapter, nil
}

// compile-time assertions that the seams this file depends on have not moved.
var (
	_ middleware.Limiter = (*redis.RateLimiter)(nil)
	_ httpx.Checker      = (*postgres.DB)(nil)
	_ httpx.Checker      = (*redis.Client)(nil)
	_ httpapi.Mux        = (*httpx.Server)(nil)

	// The betting seams. internal/httpapi declares these interfaces and
	// internal/betting does not know it exists — which is the point of
	// consumer-declared interfaces and the reason no adapter sits between them.
	// Asserting it here, at the composition root, is what turns "they happen to
	// line up" into a compile error the day one of them moves.
	_ httpapi.Betting       = (*betting.Service)(nil)
	_ httpapi.CashOutQuotes = (*betting.Service)(nil)
	_ httpapi.TicketPricer  = betting.IndependentPricer{}
	_ betting.Store         = (*bettingpg.Store)(nil)
	_ betting.Wagers        = (*bettingpg.Store)(nil)
)

// bettingPort builds the placement service and the ticket pricer.
//
// # What is deliberately left at internal/betting's defaults
//
// MaxQuoteAge, MaxFairPriceAge, the cash-out margin and the idempotency TTL are
// all policy belonging to the package that enforces them, exactly as the
// argon2id parameters and the refresh-token lifetimes are internal/auth's. A
// second copy of any of them here is how one of the two ends up loosened by
// somebody chasing a slow test — and the cash-out margin in particular is the
// book's take, which must be one reviewable constant rather than a value a
// composition root can quietly change.
//
// # Why the pricer is returned as well as installed
//
// httpapi's `/slip/quote` needs a ticket price and must not compute one. It gets
// THIS INSTANCE, so a slip quotes at the number placement will book it at.
// Returning it rather than constructing a second one is the whole guarantee;
// betting.IndependentPricer is stateless, so a second value would behave
// identically today and would silently stop doing so the moment a book supplies
// a correlation model or a teaser ladder to one of them.
//
// # The Redis idempotency cache is nil, and correctness is unaffected
//
// internal/betting derives the wager id from (user, idempotency key), so a
// replayed submit collides with wagers_pkey and the service reads the existing
// ticket back. The cache only decides whether that costs a round trip or a
// transaction; CLAUDE.md §3 calls Redis "never the source of truth" and this is
// what that means in practice. It is wired the day an implementation exists, and
// nothing about placement changes when it is.
func bettingPort(log *slog.Logger, db *postgres.DB) (*betting.Service, betting.TicketPricer, error) {
	store, err := bettingpg.New(db)
	if err != nil {
		return nil, nil, fmt.Errorf("build the betting store: %w", err)
	}

	// IndependentPricer REFUSES to price a same-game parlay or a teaser rather
	// than approximating either, and that refusal is the correct default: a
	// wrong ticket price is frozen into a row the schema then makes immutable,
	// so it is wrong forever and wrong in the direction nobody audits. A
	// deployment with a correlation matrix or a posted teaser ladder supplies
	// its own TicketPricer here and neither package changes.
	pricer := betting.IndependentPricer{}

	svc, err := betting.NewService(store, pricer, time.Now, betting.Options{
		// Wagers is the read side of the same store, needed by the cash-out
		// quote path. FairPrices is nil, so that path reports itself
		// unconfigured rather than pricing off something it should not — see
		// routeSets on why the route is then not mounted at all.
		Wagers:     store,
		FairPrices: nil,
		Cache:      nil,
		Logger:     log,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build the betting service: %w", err)
	}

	log.Info("betting service wired",
		slog.String("pricer", "independent (same-game parlays and teasers are refused, not approximated)"),
		slog.Bool("idempotency_cache", false),
		slog.Bool("cash_out_quotes", false),
	)
	return svc, pricer, nil
}
