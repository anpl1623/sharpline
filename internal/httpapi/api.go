package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/anpl1623/sharpline/internal/httpapi/gen"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

// API is the handler set for Sharpline's public REST surface.
//
// It implements [RouteSet]; server.go composes it with the middleware chain and
// mounts it. It holds no mutable state and every dependency is injected
// (CLAUDE.md §12), so a second replica behaves identically and a test can build
// one with fakes and no I/O.
//
// # What is deliberately NOT here
//
// Any decision about money. Phase 8 added the bet slip, placement, wager history
// and cash-out, and every RULE behind them — self-exclusion, responsible-gaming
// limits, the balance fold, price-move detection, the double-entry stake
// movement — lives in internal/betting, inside one transaction, in an order that
// is load-bearing. This package parses, maps sentinels onto statuses, and
// renders. It re-checks nothing, because a second answer to any of those
// questions is a second answer that can disagree with the one the transaction
// used.
//
// The same rule explains the two OPTIONAL betting ports. Where a capability's
// adapter does not exist yet, its route is NOT MOUNTED rather than mounted over
// a stub: a route that exists and does nothing is worse than an absent one,
// because a client discovers it and writes against it. [API.Routes] reports
// which shape it built and NewAPI logs it.
type API struct {
	catalogue Catalogue
	prices    Prices
	cache     PriceCache
	ledger    Ledger
	accounts  Accounts
	limits    Limits
	sessions  Sessions
	audit     Audit

	betting       Betting
	wagers        Wagers
	pricer        TicketPricer
	cashOutQuotes CashOutQuotes
	cashOuts      CashOuts

	signals     Signals
	clv         CLV
	leaderboard Leaderboard

	requireAuth []Middleware

	log   *slog.Logger
	now   Clock
	specs []byte
}

// APIOptions configures [NewAPI]. Every field but Cache and Audit is required;
// a nil dependency is a startup failure, not a nil-pointer panic on the first
// request that needs it.
type APIOptions struct {
	Catalogue Catalogue
	Prices    Prices
	Ledger    Ledger
	Accounts  Accounts
	Limits    Limits

	// Sessions is registration, login, refresh-token rotation and TOTP
	// enrolment. It is the ONE optional port, and the exception is deliberate.
	//
	// When it is nil the four /auth routes and the three /account/totp routes
	// are NOT MOUNTED, and everything else — the whole read surface, the
	// account profile, the balance and the limits — serves normally. Those
	// paths then answer the spec's own 404 envelope.
	//
	// The alternative shapes are both worse. Requiring it would make the entire
	// public catalogue dark until an unrelated adapter exists, which is a large
	// working surface withheld for no benefit. Mounting a stub that returned a
	// fabricated session would be a lie, and CLAUDE.md is explicit that a route
	// which exists and does nothing is worse than an absent one.
	//
	// [API.Routes] reports which shape it built, and NewAPI logs it at WARN, so
	// a deployment missing the adapter says so rather than being discovered by
	// a user who cannot log in.
	Sessions Sessions

	// Betting, Wagers and Pricer are the phase 8 write and read surface, and
	// all three are REQUIRED. Unlike Sessions there is no argument for
	// degrading here: an `api` that serves the catalogue and cannot take a bet
	// is not a smaller version of this service, it is a different one, and the
	// failure would be invisible because every read endpoint would stay green.
	//
	// Pricer must be THE SAME TicketPricer instance the placement service
	// holds. Two pricers would make `/slip/quote` a polite fiction: a slip
	// would quote at one number and place at another, and the difference would
	// surface to the customer as a price move that never happened.
	Betting Betting
	Wagers  Wagers
	Pricer  TicketPricer

	// CashOutQuotes prices an early close, and is OPTIONAL for one reason: it
	// needs devigged reference prices (betting.FairPrices), and a deployment
	// whose pricer is not publishing them cannot quote a cash-out at all. When
	// it is nil, `GET /wagers/{wagerId}/cashout` is not mounted and everything
	// else serves normally.
	//
	// Wiring it over a service that has no fair-price source would be the worse
	// shape: every call would answer 500, which is a mounted route that does
	// nothing — the thing this package refuses to ship.
	CashOutQuotes CashOutQuotes

	// CashOuts EXECUTES an early close, and is OPTIONAL for a different and
	// more structural reason: taking a cash-out is a state transition on a
	// placed ticket, and every other transition belongs to internal/settlement,
	// deliberately — a component that could both quote and take a cash-out
	// could do both in one transaction at a price of its own choosing. When it
	// is nil, `POST /wagers/{wagerId}/cashout` is not mounted and the quote on
	// the same path still serves.
	CashOuts CashOuts

	// Signals, CLV and Leaderboard are the phase 9 analytics read surface, and
	// all three are REQUIRED.
	//
	// Unlike Sessions and the two cash-out ports there is no adapter-availability
	// question to degrade around: they read tables the same migration set that
	// creates `events` and `prices` also creates, over the same pool, through
	// the same generated queries. A deployment could not have one of these
	// without the others and could not have the board without all three.
	//
	// Making them required is also what stops the analytics surface going dark
	// by omission. CLAUDE.md §6 calls analytics "the differentiator"; a wire-up
	// that forgot one of these ports would serve a complete-looking product
	// whose most distinctive feature answered 404, and nothing in the logs or
	// the tests would say why. A startup error is the loud version of that.
	Signals     Signals
	CLV         CLV
	Leaderboard Leaderboard

	// Cache is the optional Redis snapshot in front of Prices. nil disables it
	// and every read goes to Postgres, which is correct — just slower.
	Cache PriceCache

	// Audit is the sink for the state-changing actions that do not already
	// write their audit row inside their own transaction. nil drops them, which
	// is acceptable ONLY in a unit test; a startup that leaves it nil in a real
	// deployment is a compliance gap and NewAPI says so in the log.
	Audit Audit

	// RequireAuth is the middleware chain that turns an anonymous request into
	// an authenticated one, built by the composition root from
	// internal/httpapi/middleware (Authenticate + RequireIdentity). It is
	// REQUIRED and NewAPI refuses an empty one.
	//
	// It is injected rather than constructed here for two reasons. The
	// verification itself — algorithm pinning, issuer, audience, expiry — is
	// internal/auth's and internal/httpapi/middleware's, and a second
	// implementation in this file would be a second place for it to be wrong.
	// And making it required means an account route CANNOT become public by
	// omission: there is no default, so a caller that forgets it gets a startup
	// error rather than an open endpoint.
	RequireAuth []Middleware

	Logger *slog.Logger

	// Now is the clock. nil means time.Now. It is injected so that "as_of" on a
	// page and the instants stamped on an audit row come from ONE reading — a
	// handler calling time.Now() three times puts three instants in one
	// response, and a test cannot pin any of them.
	Now Clock
}

// NewAPI validates the dependency set and returns the handler set.
func NewAPI(opts APIOptions) (*API, error) {
	missing := []string{}
	if opts.Catalogue == nil {
		missing = append(missing, "Catalogue")
	}
	if opts.Prices == nil {
		missing = append(missing, "Prices")
	}
	if opts.Ledger == nil {
		missing = append(missing, "Ledger")
	}
	if opts.Accounts == nil {
		missing = append(missing, "Accounts")
	}
	if opts.Limits == nil {
		missing = append(missing, "Limits")
	}
	if opts.Betting == nil {
		missing = append(missing, "Betting")
	}
	if opts.Wagers == nil {
		missing = append(missing, "Wagers")
	}
	if opts.Pricer == nil {
		missing = append(missing, "Pricer")
	}
	if opts.Signals == nil {
		missing = append(missing, "Signals")
	}
	if opts.CLV == nil {
		missing = append(missing, "CLV")
	}
	if opts.Leaderboard == nil {
		missing = append(missing, "Leaderboard")
	}
	if len(opts.RequireAuth) == 0 {
		// Not a nil check with a default: a default here would make every
		// account route public the moment a caller forgot the field.
		missing = append(missing, "RequireAuth")
	}
	if opts.Logger == nil {
		missing = append(missing, "Logger")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: APIOptions is missing %s",
			ErrInvalidOptions, strings.Join(missing, ", "))
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	spec, err := SpecBytes()
	if err != nil {
		return nil, fmt.Errorf("httpapi: load embedded spec: %w", err)
	}

	if opts.Audit == nil {
		opts.Logger.Warn("audit sink is nil: state-changing actions will not be recorded",
			slog.String("requirement", "CLAUDE.md §6 platform: audit log on every state-changing action"))
	}
	if opts.CashOutQuotes == nil {
		opts.Logger.Warn("no cash-out pricer: the cash-out quote route is not mounted",
			slog.String("unmounted", "GET /v1/wagers/{wagerId}/cashout"),
			slog.String("needs", "a betting.FairPrices source — devigged reference prices "+
				"from internal/pricing, per ADR 0006"),
			slog.String("effect", "placement, history and the wager detail page are unaffected"))
	}
	if opts.CashOuts == nil {
		opts.Logger.Warn("no cash-out executor: the take-cash-out route is not mounted",
			slog.String("unmounted", "POST /v1/wagers/{wagerId}/cashout"),
			slog.String("why", "settling a ticket early is a state transition and belongs with "+
				"the other transitions in internal/settlement"),
			slog.String("effect", "that path answers the spec's own 404; nothing is fabricated"))
	}
	if opts.Sessions == nil {
		opts.Logger.Warn("no session adapter: the auth and TOTP routes are not mounted",
			slog.String("unmounted", "POST /v1/auth/{register,login,refresh,logout}, /v1/account/totp*"),
			slog.String("effect", "those paths answer the spec-shaped 404; nothing is fabricated"))
	}

	return &API{
		catalogue: opts.Catalogue,
		prices:    opts.Prices,
		cache:     opts.Cache,
		ledger:    opts.Ledger,
		accounts:  opts.Accounts,
		limits:    opts.Limits,
		sessions:  opts.Sessions,
		audit:     opts.Audit,

		betting:       opts.Betting,
		wagers:        opts.Wagers,
		pricer:        opts.Pricer,
		cashOutQuotes: opts.CashOutQuotes,
		cashOuts:      opts.CashOuts,

		signals:     opts.Signals,
		clv:         opts.CLV,
		leaderboard: opts.Leaderboard,

		requireAuth: slices.Clone(opts.RequireAuth),

		log:   opts.Logger,
		now:   now,
		specs: spec,
	}, nil
}

// Routes implements [RouteSet].
//
// THIS LIST AND openapi.yaml ARE THE SAME SET, and routes_test.go proves it:
// every (method, path) here appears in the spec, and every path in the spec
// appears here. A handler mounted at an undeclared path fails the test, and so
// does a path declared in the spec with no handler behind it. That is what makes
// the spec a contract rather than documentation.
//
// The paths are relative to the API root; server.go prepends the public prefix,
// so no handler hardcodes "/api" and moving the prefix stays a one-line change.
func (a *API) Routes() []Route {
	authed := a.requireAuth

	routes := []Route{
		// The spec serves itself. A client generating a stub from a copied file
		// is generating from a file that may be older than the server; this is
		// always the running build's own contract.
		{Method: http.MethodGet, Path: "/openapi.yaml", Handler: http.HandlerFunc(a.handleSpec)},

		// Catalogue
		{Method: http.MethodGet, Path: "/v1/sports", Handler: http.HandlerFunc(a.handleListSports)},
		{Method: http.MethodGet, Path: "/v1/sports/{sportSlug}/leagues", Handler: http.HandlerFunc(a.handleListLeagues)},
		{Method: http.MethodGet, Path: "/v1/books", Handler: http.HandlerFunc(a.handleListBooks)},
		{Method: http.MethodGet, Path: "/v1/events/{eventId}", Handler: http.HandlerFunc(a.handleGetEvent)},
		{Method: http.MethodGet, Path: "/v1/markets/{marketId}/prices", Handler: http.HandlerFunc(a.handleCompareMarket)},
		{Method: http.MethodGet, Path: "/v1/search", Handler: http.HandlerFunc(a.handleSearch)},

		// Board
		{Method: http.MethodGet, Path: "/v1/board", Handler: http.HandlerFunc(a.handleBoard)},
		{Method: http.MethodGet, Path: "/v1/leagues/{leagueSlug}/board", Handler: http.HandlerFunc(a.handleLeagueBoard)},

		// History
		{Method: http.MethodGet, Path: "/v1/selections/{selectionId}/history", Handler: http.HandlerFunc(a.handleHistory)},

		// Signals and the leaderboard. Public and unauthenticated, like the
		// board: they are derived from public odds, and the leaderboard names
		// nobody — its rows carry a derived pseudonym rather than an account.
		{Method: http.MethodGet, Path: "/v1/signals/ev", Handler: http.HandlerFunc(a.handleEVSignals)},
		{Method: http.MethodGet, Path: "/v1/signals/arbitrage", Handler: http.HandlerFunc(a.handleArbitrageSignals)},
		{Method: http.MethodGet, Path: "/v1/signals/steam", Handler: http.HandlerFunc(a.handleSteamSignals)},
		{Method: http.MethodGet, Path: "/v1/leaderboard", Handler: http.HandlerFunc(a.handleLeaderboard)},

		// Account. Every one carries the authentication middleware; there is no
		// route below that resolves a user id from a path or a body, so a
		// handler CANNOT act on a user other than the one the token names.
		{Method: http.MethodGet, Path: "/v1/account", Handler: http.HandlerFunc(a.handleGetAccount), Middleware: authed},
		{Method: http.MethodGet, Path: "/v1/account/balance", Handler: http.HandlerFunc(a.handleGetBalance), Middleware: authed},
		{Method: http.MethodPost, Path: "/v1/account/grant", Handler: http.HandlerFunc(a.handleGrant), Middleware: authed},
		{Method: http.MethodPost, Path: "/v1/account/self-exclusion", Handler: http.HandlerFunc(a.handleSelfExclude), Middleware: authed},
		{Method: http.MethodGet, Path: "/v1/account/limits", Handler: http.HandlerFunc(a.handleListLimits), Middleware: authed},
		{Method: http.MethodPost, Path: "/v1/account/limits", Handler: http.HandlerFunc(a.handleSetLimit), Middleware: authed},
		{Method: http.MethodGet, Path: "/v1/account/clv", Handler: http.HandlerFunc(a.handleAccountCLV), Middleware: authed},

		// Betting. Authenticated for the same reason the account routes are,
		// and with the same property: no path parameter, query parameter or
		// body field anywhere below names a user. `/wagers/{wagerId}` names a
		// WAGER, which is the one identifier here that belongs to somebody —
		// so the read that resolves it takes the caller's id and answers
		// [ErrNotFound] for a ticket that is not theirs, indistinguishably
		// from one that does not exist.
		{Method: http.MethodPost, Path: "/v1/slip/quote", Handler: http.HandlerFunc(a.handleQuoteSlip), Middleware: authed},
		{Method: http.MethodGet, Path: "/v1/wagers", Handler: http.HandlerFunc(a.handleListWagers), Middleware: authed},
		{Method: http.MethodPost, Path: "/v1/wagers", Handler: http.HandlerFunc(a.handlePlaceWager), Middleware: authed},
		{Method: http.MethodGet, Path: "/v1/wagers/{wagerId}", Handler: http.HandlerFunc(a.handleGetWager), Middleware: authed},
	}

	// The two cash-out routes are mounted per PORT rather than as a pair,
	// because the two capabilities are genuinely independent: pricing an early
	// close needs devigged reference prices, taking one needs the settlement
	// write path, and a deployment can plausibly have either without the other.
	if a.cashOutQuotes != nil {
		routes = append(routes, Route{
			Method: http.MethodGet, Path: "/v1/wagers/{wagerId}/cashout",
			Handler: http.HandlerFunc(a.handleCashOutQuote), Middleware: authed,
		})
	}
	if a.cashOuts != nil {
		routes = append(routes, Route{
			Method: http.MethodPost, Path: "/v1/wagers/{wagerId}/cashout",
			Handler: http.HandlerFunc(a.handleTakeCashOut), Middleware: authed,
		})
	}

	if a.sessions == nil {
		return routes
	}

	return append(routes,
		// Auth. Unauthenticated by construction — these are how a credential is
		// obtained, so requiring one would be circular.
		Route{Method: http.MethodPost, Path: "/v1/auth/register", Handler: http.HandlerFunc(a.handleRegister)},
		Route{Method: http.MethodPost, Path: "/v1/auth/login", Handler: http.HandlerFunc(a.handleLogin)},
		Route{Method: http.MethodPost, Path: "/v1/auth/refresh", Handler: http.HandlerFunc(a.handleRefresh)},
		Route{Method: http.MethodPost, Path: "/v1/auth/logout", Handler: http.HandlerFunc(a.handleLogout)},

		// TOTP enrolment is a session operation even though it hangs off
		// /account: minting and proving a shared secret is internal/auth's, so
		// these travel with the session adapter rather than with the profile.
		Route{Method: http.MethodPost, Path: "/v1/account/totp", Handler: http.HandlerFunc(a.handleBeginTOTP), Middleware: authed},
		Route{Method: http.MethodPost, Path: "/v1/account/totp/confirm", Handler: http.HandlerFunc(a.handleConfirmTOTP), Middleware: authed},
		Route{Method: http.MethodDelete, Path: "/v1/account/totp", Handler: http.HandlerFunc(a.handleRemoveTOTP), Middleware: authed},
	)
}

// HasCashOutQuotes and HasTakeCashOut report which of the optional betting
// routes are mounted, so the composition root can log the shape it built rather
// than leaving "why does cash-out 404" to be answered by reading the route
// table.
func (a *API) HasCashOutQuotes() bool { return a.cashOutQuotes != nil }

// HasTakeCashOut reports whether POST /wagers/{wagerId}/cashout is mounted.
func (a *API) HasTakeCashOut() bool { return a.cashOuts != nil }

// HasSessions reports whether the session-dependent routes are mounted.
//
// The composition root logs this at startup so "why does login 404" is
// answerable from the first ten lines of a container's output rather than from
// reading the route table.
func (a *API) HasSessions() bool { return a.sessions != nil }

// -----------------------------------------------------------------------------
// Authentication
// -----------------------------------------------------------------------------

// caller returns the authenticated user, or answers 401 and reports false.
//
// THE ONLY WAY A HANDLER LEARNS WHO IS CALLING. It reads the identity
// internal/httpapi/middleware established; there is no path parameter, no query
// parameter and no body field anywhere in this API that names a user, so
// "act on behalf of another user" is NOT EXPRESSIBLE rather than merely
// forbidden. That is why none of the account handlers has an authorization
// check in it — there is nothing for one to compare.
//
// The false branch is defence in depth. Every account route carries
// [APIOptions.RequireAuth], so an anonymous request should never reach a handler
// that calls this; if the route table and the middleware ever disagree, this
// answers 401 rather than serving somebody else's account with a zero user id.
func (a *API) caller(w http.ResponseWriter, r *http.Request) (domain.UserID, bool) {
	id, ok := middleware.IdentityFrom(r.Context())
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="sharpline", error="invalid_token"`)
		fail(w, r, http.StatusUnauthorized, gen.ErrorCodeUnauthenticated, msgUnauthenticated)
		return "", false
	}
	return id.UserID, true
}

// -----------------------------------------------------------------------------
// Shared handler plumbing
// -----------------------------------------------------------------------------

// auditContext assembles the provenance for a state-changing action.
//
// The client address comes from [middleware.ClientIPFrom], which resolved it
// against the trusted-proxy list — never from a header read here, which any
// client could forge to attribute its actions to somebody else's address.
func (a *API) auditContext(r *http.Request) AuditContext {
	ac := AuditContext{
		RequestID: middleware.RequestIDFrom(r.Context()),
		At:        a.now().UTC(),
	}
	if ip, ok := middleware.ClientIPFrom(r.Context()); ok {
		ac.ClientIP = ip
	}
	if sc := traceIDs(r.Context()); sc.trace != "" {
		ac.TraceID = sc.trace
		ac.SpanID = sc.span
	}
	return ac
}

// record writes an audit entry, and never fails a request because of it.
//
// A state-changing action whose audit row could not be written is a real
// problem, but it is an OPERATIONAL one: the change already committed, and
// answering 500 would tell the client the action failed when it did not, which
// is the one outcome worse than a missing audit row. So it is logged at error —
// loudly, joinable by request id — and the response stands.
//
// The actions that must NOT behave this way are the ones whose audit row belongs
// to the same transaction as the change. Those go through the store (see
// [Limits.Set]) and commit or roll back together; this function is only for the
// actions with no transaction of their own.
func (a *API) record(ctx context.Context, e AuditEntry) {
	if a.audit == nil {
		return
	}
	if err := a.audit.Record(ctx, e); err != nil {
		a.log.ErrorContext(ctx, "audit entry not recorded",
			slog.String("action", e.Action),
			slog.String("entity_type", e.EntityType),
			slog.String("request_id", e.Context.RequestID),
			slog.String("error", err.Error()))
	}
}

// notFoundOr answers 404 for a missing row and 500 for anything else.
//
// The store reports a missing row as [ErrNotFound], which this package declares
// and the adapter wraps pgx.ErrNoRows into. Collapsing every store error into 404 would
// turn a database outage into "no such event", which is the kind of lie that
// makes an outage take an hour to find.
func (a *API) notFoundOr(w http.ResponseWriter, r *http.Request, op string, err error) {
	if errors.Is(err, ErrNotFound) {
		failNotFound(w, r)
		return
	}
	failWith(w, r, a.log, op, err)
}
