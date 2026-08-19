package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Context keys are unexported empty structs, so nothing outside this package can
// forge a value under one. That matters most for identityKey: if a handler in
// another package could write an Identity into the context, authentication would
// be advisory.
type (
	requestIDKey struct{}
	identityKey  struct{}
	loggerKey    struct{}
	clientIPKey  struct{}
	routeKey     struct{}
)

// RequestID returns the id assigned to this request, or "" if the request did
// not pass through the RequestID middleware.
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

// Identity is the authenticated caller.
//
// It carries identity, not credentials. There is no token field and there will
// not be one: a struct that holds the raw access token is a struct that
// eventually gets logged, put on a span, or serialised into an error. The token
// is verified once, in the Authenticate middleware, and discarded there.
//
// The fields are the ones the schema already models (migrations/00005). There is
// deliberately no email, no role and no balance:
//
//   - email is a personal identifier that would end up in every access-log line
//     for no operational gain — the user id is the join key;
//   - roles do not exist in the schema; when the admin console (CLAUDE.md §6,
//     Platform) needs one, it is an authorisation lookup, not a token claim
//     this middleware invents;
//   - a balance is derived from ledger_entries and is never carried as a
//     mutable field (CLAUDE.md §4).
type Identity struct {
	// UserID is users.id.
	UserID domain.UserID

	// SessionID is the refresh_token_families row this access token descends
	// from. It is what lets an audit row name the session as well as the user,
	// and what a "log out this device" action revokes. Empty is legal — a token
	// minted outside a family (an operator token) has none.
	SessionID string

	// IssuedAt and ExpiresAt are the token's own bounds, already verified by
	// the Authenticator. They are here so a handler can reason about remaining
	// lifetime without re-parsing anything.
	IssuedAt  time.Time
	ExpiresAt time.Time

	// AMR lists the authentication methods that produced this token — "pwd",
	// "totp". CLAUDE.md §6 makes TOTP optional per user, so an endpoint that
	// wants to insist on it (changing a password, raising a self-imposed limit)
	// checks here rather than re-authenticating.
	AMR []string
}

// IsZero reports whether the identity is unset — i.e. the request is anonymous.
func (i Identity) IsZero() bool { return i.UserID.IsZero() }

// HasAMR reports whether the token was minted with the named authentication
// method.
func (i Identity) HasAMR(method string) bool {
	for _, m := range i.AMR {
		if m == method {
			return true
		}
	}
	return false
}

// LogValue implements slog.LogValuer.
//
// It exists so that logging a whole Identity is safe by construction. Every
// field is already non-secret, and keeping the rendering here means a field
// added later is rendered by a function somebody has to edit rather than being
// swept into a log line by reflection.
func (i Identity) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("user_id", i.UserID.String()),
		slog.String("session_id", i.SessionID),
	)
}

// IdentityFrom returns the authenticated caller, and whether there is one.
//
// A false second return means the request is anonymous — either no credential
// was presented, or the chain did not include Authenticate. It never means
// "a credential was presented and rejected": that request was answered with 401
// and never reached a handler.
func IdentityFrom(ctx context.Context) (Identity, bool) {
	c, ok := ctx.Value(identityKey{}).(*identityCell)
	if !ok || c == nil || !c.isSet || c.id.IsZero() {
		return Identity{}, false
	}
	return c.id, true
}

// identityCell holds the authenticated caller behind a pointer.
//
// The indirection is not incidental. Authentication happens deep in the chain,
// but the ACCESS LOG is written by a middleware near the top — and that
// middleware is blocked inside next.ServeHTTP by the time the identity exists,
// holding the *http.Request from before the identity was attached. A plain
// context value would therefore be invisible to it, and every authenticated
// request would be logged as anonymous. The cell is reachable from the outer
// context, so the value written at the bottom is readable at the top.
//
// Same mechanism, same reason, as routeCell. Requests are handled by one
// goroutine, so no lock is needed.
type identityCell struct {
	id    Identity
	isSet bool
}

// ensureIdentityCell returns the context's identity cell, installing one if
// there is none. Idempotent.
func ensureIdentityCell(ctx context.Context) (context.Context, *identityCell) {
	if c, ok := ctx.Value(identityKey{}).(*identityCell); ok && c != nil {
		return ctx, c
	}
	c := &identityCell{}
	return context.WithValue(ctx, identityKey{}, c), c
}

// withIdentity is unexported on purpose: only Authenticate may establish an
// identity. Exporting it would make every handler a place where authentication
// could be forged.
func withIdentity(ctx context.Context, id Identity) context.Context {
	ctx, cell := ensureIdentityCell(ctx)
	cell.id = id
	cell.isSet = true
	return ctx
}

// Logger returns the request-scoped logger: the injected base logger with
// request_id, trace_id, span_id and (once authenticated) user_id already
// attached.
//
// It falls back to slog.Default only when the chain did not include Correlate,
// which is the case in a unit test that exercises one handler in isolation. A
// handler should always take this rather than closing over the service logger,
// because a line without the correlation attributes cannot be joined to the
// trace that produced it.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// ClientIP returns the address the chain attributed this request to, and
// whether it could be determined.
//
// See clientip.go: behind the proxy this is derived from a forwarded header,
// and ONLY when the direct peer is in the configured trusted-proxy set. A
// zero-value address means neither RemoteAddr nor any trusted header parsed —
// which is possible on a unix-socket listener and in a synthesised test request.
func ClientIPFrom(ctx context.Context) (netip.Addr, bool) {
	addr, ok := ctx.Value(clientIPKey{}).(netip.Addr)
	return addr, ok && addr.IsValid()
}

func withClientIP(ctx context.Context, addr netip.Addr) context.Context {
	return context.WithValue(ctx, clientIPKey{}, addr)
}

// Route returns the route pattern this request matched, or "" if it was not
// resolved.
//
// It is the bounded label every HTTP metric is broken down by; see RouteFunc for
// why a raw path can never be used in its place.
func RouteFrom(ctx context.Context) string {
	c, ok := ctx.Value(routeKey{}).(*routeCell)
	if !ok || c == nil {
		return ""
	}
	return c.get()
}

// SetRoute records the route pattern for this request, for a router that is not
// an *http.ServeMux and therefore cannot be handled by MuxRouteFunc.
//
// It mutates a cell placed in the context by the Metrics middleware, rather than
// deriving a new context, because by the time a router knows the pattern the
// outer middleware is already blocked in ServeHTTP and can no longer see a
// replaced context. Calling it when the chain did not install a cell is a no-op.
func SetRoute(r *http.Request, pattern string) {
	c, ok := r.Context().Value(routeKey{}).(*routeCell)
	if ok && c != nil {
		c.set(pattern)
	}
}

// routeCell is a single-assignment box. Requests are handled by one goroutine,
// so no lock is needed; the pointer indirection is the whole point — it survives
// the request clone that http.ServeMux makes when it records its matched
// pattern.
type routeCell struct{ v string }

func (c *routeCell) set(s string) {
	if c.v == "" {
		c.v = s
	}
}

func (c *routeCell) get() string { return c.v }

func withRouteCell(ctx context.Context, c *routeCell) context.Context {
	return context.WithValue(ctx, routeKey{}, c)
}

// ensureRouteCell returns the context's route cell, installing one if there is
// none. Idempotent, so ResolveRoute and a standalone Metrics middleware cannot
// end up with two cells where the inner one is the only one anybody writes to.
func ensureRouteCell(ctx context.Context) (context.Context, *routeCell) {
	if c, ok := ctx.Value(routeKey{}).(*routeCell); ok && c != nil {
		return ctx, c
	}
	c := &routeCell{}
	return withRouteCell(ctx, c), c
}
