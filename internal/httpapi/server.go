// The composition root of Sharpline's public REST surface: the route table, the
// mount onto the operational listener, the error writer that makes every error
// in this service match the OpenAPI contract, and the answers for a path no
// route claims.
//
// It owns COMPOSITION and nothing else. There is no handler here, no query and
// no token verification; those live in sibling files. There is also no request
// id, no client-IP resolution, no access log, no metric, no panic recovery and
// no body cap — internal/httpapi/middleware owns all of those and argues each
// position in the chain, and duplicating any of them here would mean two places
// decide one security property. Middleware arrives through [Options.Middleware]
// already assembled.
//
// # What this file actually decides
//
//  1. THE PREFIX IS PREPENDED IN ONE PLACE. deploy/proxy/Caddyfile forwards
//     /api/* here WITHOUT stripping, and openapi.yaml says the same thing from
//     the other side with `servers: - url: /api/v1`. So a [Route] carries the
//     path relative to the prefix — "/v1/events/{id}" — and no handler ever
//     hardcodes "/api". Moving the prefix is a one-line change rather than a
//     sweep, and the route table cannot disagree with the proxy.
//
//  2. A WRONG METHOD IS A 405, NOT A 404. net/http produces a correct 405 by
//     itself, but only while no subtree pattern matches — and this server has
//     to register "/api/" so that an unmatched path gets a spec-shaped JSON
//     body instead of net/http's plain text. [Server.handleUnmatched] recovers
//     the right answer from a second, method-free mux rather than
//     re-implementing pattern matching.
//
//  3. EVERY ERROR IN THIS SERVICE HAS ONE SHAPE. internal/httpapi/middleware
//     deliberately does not dictate the envelope; it takes an ErrorWriter and
//     says "internal/httpapi owns the public schema and its OpenAPI document".
//     [WriteAPIError] is that writer. It renders gen.Error — the type generated
//     FROM openapi.yaml — so the chain's 401, the limiter's 429, this file's
//     404 and a handler's 422 are all the same four keys.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/anpl1623/sharpline/internal/httpapi/gen"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

// Frozen contract with deploy/proxy/Caddyfile, openapi.yaml and the Helm
// Ingress.
const (
	// DefaultPublicPrefix is the path prefix Caddy forwards to this service
	// WITHOUT stripping (Caddyfile, Route 2: "the prefix is PRESERVED"). Every
	// public route mounts beneath it.
	DefaultPublicPrefix = "/api"

	// APIVersion is the first path segment below the prefix, so a [Route] path
	// begins "/v1/". openapi.yaml declares `servers: - url: /api/v1`; the two
	// halves of that string live here and there and must stay equal.
	APIVersion = "v1"

	// VersionPrefix is the two joined, for a caller that wants to build an
	// absolute path for a log line or a test.
	VersionPrefix = DefaultPublicPrefix + "/" + APIVersion
)

// ErrInvalidOptions is returned by [New] when [Options] cannot produce a usable
// server. Callers match it with errors.Is.
var ErrInvalidOptions = errors.New("httpapi: invalid server options")

// Middleware is one link in the chain. It is an alias rather than a new type so
// that a middleware built by internal/httpapi/middleware is usable here and in
// [Route.Middleware] without conversion, and so there is one middleware type in
// the service rather than two that need adapting at every boundary.
type Middleware = middleware.Middleware

// Route is one entry in the API's route table.
type Route struct {
	// Method is a single HTTP method, e.g. http.MethodGet. Required.
	//
	// Registering GET also serves HEAD — net/http's pattern matcher does that
	// for us — so there is no separate HEAD route to write.
	Method string

	// Path is the route path RELATIVE TO THE PUBLIC PREFIX, starting with a
	// slash and, in practice, with the version: "/v1/events/{id}". net/http
	// pattern syntax applies, so "{id}" is a segment wildcard and "{rest...}" a
	// trailing one.
	Path string

	// Handler serves the route. Required.
	Handler http.Handler

	// Middleware wraps this route only, applied outermost-first, INSIDE the
	// server-wide chain. Authentication belongs here: it must run after the
	// client IP is resolved and after the per-IP limiter, and only on the
	// routes that require it. A route left without it is public, which is the
	// correct default for the catalogue and the board.
	Middleware []Middleware
}

// RouteSet is the consumer-declared seam over anything that contributes routes
// (CLAUDE.md §12: "Interfaces are declared by the consumer, not the producer").
// A handler struct implements it with one method and this file never learns its
// type.
type RouteSet interface {
	Routes() []Route
}

// Mux is the consumer-declared seam over the thing this server mounts onto.
// *httpx.Server satisfies it, so this package does not import
// internal/platform/httpx and that package does not import this one.
type Mux interface {
	Handle(pattern string, h http.Handler)
}

// Options configures the public API server. Everything is injected; the package
// holds no global state (CLAUDE.md §12).
type Options struct {
	// PublicPrefix is the path prefix the proxy forwards without stripping.
	// Empty means [DefaultPublicPrefix].
	PublicPrefix string

	// Routes and RouteSets together form the route table. Both are accepted so
	// a caller can pass a handler struct without flattening it and can still
	// add a one-off route without inventing a type for it.
	Routes    []Route
	RouteSets []RouteSet

	// Middleware wraps EVERY request beneath the prefix, including one that
	// matches no route — which is what the per-IP rate limiter needs, since a
	// scanner hammering paths that do not exist is exactly the traffic a
	// per-route limiter cannot see. In cmd/api this is the assembled stack from
	// internal/httpapi/middleware; the order within it is decided there.
	Middleware []Middleware

	// ErrorWriter renders the 404 and 405 this file produces. nil means
	// [WriteAPIError], which is the spec-shaped writer the whole service should
	// be using. It is injectable only so a test can observe what was written
	// without parsing it back.
	ErrorWriter middleware.ErrorWriter
}

// Server is the public REST surface. Safe for concurrent use: it holds no
// per-request state and nothing a second replica could not reconstruct, which
// is what CLAUDE.md §9 requires of a Deployment behind an Ingress.
type Server struct {
	prefix  string
	handler http.Handler

	// patterns is the absolute "METHOD /api/v1/..." form of every route,
	// sorted, for the startup log line.
	patterns []string

	// methodIndex answers 405. See [Server.handleUnmatched].
	methodIndex *http.ServeMux

	writeErr middleware.ErrorWriter
}

// New builds the public API server. It binds no socket and starts no goroutine;
// call [Server.Mount] to attach it to a listener.
func New(opts Options) (*Server, error) {
	prefix := strings.TrimRight(opts.PublicPrefix, "/")
	if prefix == "" {
		prefix = DefaultPublicPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("%w: PublicPrefix %q does not start with a slash", ErrInvalidOptions, opts.PublicPrefix)
	}

	writeErr := opts.ErrorWriter
	if writeErr == nil {
		writeErr = WriteAPIError
	}

	s := &Server{
		prefix:      prefix,
		methodIndex: http.NewServeMux(),
		writeErr:    writeErr,
	}

	routes, err := collectRoutes(opts.Routes, opts.RouteSets)
	if err != nil {
		return nil, err
	}

	inner := http.NewServeMux()
	// allowed maps an absolute path pattern to the methods registered on it, so
	// a 405 carries a truthful Allow header.
	allowed := make(map[string][]string, len(routes))
	seen := make(map[string]struct{}, len(routes))

	for _, rt := range routes {
		if rt.Method == "" {
			return nil, fmt.Errorf("%w: route %q has no method", ErrInvalidOptions, rt.Path)
		}
		if !strings.HasPrefix(rt.Path, "/") {
			return nil, fmt.Errorf("%w: route path %q does not start with a slash", ErrInvalidOptions, rt.Path)
		}
		if rt.Handler == nil {
			return nil, fmt.Errorf("%w: route %s %s has a nil handler", ErrInvalidOptions, rt.Method, rt.Path)
		}

		absolute := prefix + rt.Path
		pattern := rt.Method + " " + absolute
		if _, dup := seen[pattern]; dup {
			return nil, fmt.Errorf("%w: route %s is registered twice", ErrInvalidOptions, pattern)
		}
		seen[pattern] = struct{}{}

		h := rt.Handler
		for i := len(rt.Middleware) - 1; i >= 0; i-- {
			if rt.Middleware[i] == nil {
				return nil, fmt.Errorf("%w: route %s has a nil middleware entry", ErrInvalidOptions, pattern)
			}
			h = rt.Middleware[i](h)
		}
		inner.Handle(pattern, withRouteLabel(absolute, h))

		allowed[absolute] = append(allowed[absolute], rt.Method)
		s.patterns = append(s.patterns, pattern)
	}

	// One entry per distinct path, methodless, carrying its method list.
	for path, methods := range allowed {
		slices.Sort(methods)
		methods = slices.Compact(methods)
		// A GET route also serves HEAD, so Allow must say so or a conditional
		// client is misinformed about what it may send.
		if slices.Contains(methods, http.MethodGet) && !slices.Contains(methods, http.MethodHead) {
			methods = append(methods, http.MethodHead)
			slices.Sort(methods)
		}
		s.methodIndex.Handle(path, methodList(methods))
	}

	inner.HandleFunc(prefix+"/", s.handleUnmatched)

	s.handler = middleware.Chain(inner, opts.Middleware...)
	slices.Sort(s.patterns)
	return s, nil
}

// collectRoutes flattens the two route sources into one slice.
func collectRoutes(direct []Route, sets []RouteSet) ([]Route, error) {
	out := slices.Clone(direct)
	for i, set := range sets {
		if set == nil {
			return nil, fmt.Errorf("%w: RouteSets[%d] is nil", ErrInvalidOptions, i)
		}
		out = append(out, set.Routes()...)
	}
	return out, nil
}

// Handler returns the composed handler for the whole public prefix subtree.
func (s *Server) Handler() http.Handler { return s.handler }

// Prefix reports the public path prefix this server mounts beneath.
func (s *Server) Prefix() string { return s.prefix }

// Patterns lists every registered route as "METHOD /api/v1/...", sorted.
//
// It is logged at startup and it is what a spec-conformance test compares
// against openapi.yaml: the runtime image is distroless, so there is no shell
// in which to ask a running container what it serves, and this is the only
// place the answer appears.
func (s *Server) Patterns() []string { return slices.Clone(s.patterns) }

// Mount attaches the API to a mux — in practice the operational listener from
// internal/platform/httpx, which already owns /healthz, /readyz and /metrics.
//
// It registers ONE subtree pattern. The two probes httpx mirrors beneath the
// prefix are more specific patterns and therefore still win, which is what
// keeps `GET /api/healthz` answering from httpx rather than 404ing here.
func (s *Server) Mount(m Mux) {
	m.Handle(s.prefix+"/", s.handler)
}

// withRouteLabel publishes the matched pattern so the metrics and access log in
// the chain OUTSIDE this router can label themselves with a bounded value.
//
// The raw path can never be that label: it is client-controlled, so a metric
// broken down by it is a memory-exhaustion bug reachable from the internet.
// middleware.SetRoute writes into a single-assignment cell the chain installed;
// when no chain is present — a unit test driving the router directly — it is a
// no-op.
func withRouteLabel(pattern string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		middleware.SetRoute(r, pattern)
		next.ServeHTTP(w, r)
	})
}

// handleUnmatched answers every request beneath the prefix that no route
// claimed: 405 when the path exists under a different method, 404 otherwise.
//
// The 405 half is the reason this exists. net/http produces a correct 405 on
// its own, but only while no subtree pattern matches — and "/api/" is
// registered precisely so an unmatched path gets a spec-shaped JSON body
// instead of plain text. Once it is registered, a GET to a POST-only path
// matches the subtree and would be answered 404, telling a client its request
// was wrong in the wrong way. methodIndex is a second, method-free mux over the
// same paths; asking net/http to match it recovers the right answer without
// this file re-implementing pattern matching, wildcards and precedence.
func (s *Server) handleUnmatched(w http.ResponseWriter, r *http.Request) {
	if h, pattern := s.methodIndex.Handler(r); pattern != "" {
		if methods, ok := h.(methodList); ok {
			middleware.SetRoute(r, pattern)
			w.Header().Set("Allow", strings.Join(methods, ", "))
			s.writeErr(w, r, ProblemMethodNotAllowed)
			return
		}
	}
	s.writeErr(w, r, ProblemNotFound)
}

// methodList is the value stored in methodIndex: the methods a path accepts. It
// satisfies http.Handler only because http.ServeMux requires one; the handler
// is never invoked, the value is read.
type methodList []string

func (m methodList) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Allow", strings.Join(m, ", "))
	w.WriteHeader(http.StatusMethodNotAllowed)
}

// The two problems composition itself produces.
//
// 405 is the awkward one. openapi.yaml's `Error.code` is a closed enum with no
// `method_not_allowed`, and emitting a code the spec does not list would break
// a client generated from it — so a wrong method is reported as `bad_request`,
// which is defensible (the request IS malformed) and coarser than it should be.
// The Allow header carries the precise answer. Reported upward as a one-line
// spec change rather than papered over here.
var (
	ProblemNotFound = middleware.Problem{
		Status:  http.StatusNotFound,
		Code:    middleware.CodeNotFound,
		Message: "No such resource.",
	}
	ProblemMethodNotAllowed = middleware.Problem{
		Status:  http.StatusMethodNotAllowed,
		Code:    middleware.CodeBadRequest,
		Message: "That method is not allowed for this resource.",
	}
)

// WriteAPIError is the service's ErrorWriter: it renders a middleware.Problem
// as the `Error` schema from openapi.yaml.
//
// internal/httpapi/middleware is explicit that it does not own the envelope —
// "internal/httpapi owns the public schema and its OpenAPI document; when it
// defines an envelope, it passes its own writer here and every error this chain
// produces matches every error a handler produces". This is that writer, and
// cmd/api hands it to the stack.
//
// It renders gen.Error, the type GENERATED FROM the spec, so the field names
// and the code enum cannot drift from the contract without failing to compile
// or failing `make codegen-check-openapi`.
//
// Message never carries interior state. middleware.Problem has no field through
// which a Go error could travel, and this function adds none — everything a
// caller must not learn is logged under the request id and dropped here.
func WriteAPIError(w http.ResponseWriter, r *http.Request, p middleware.Problem) {
	body := gen.Error{
		Code:      specCode(p.Code),
		Message:   p.Message,
		RequestId: middleware.RequestIDFrom(r.Context()),
	}

	buf, err := json.Marshal(body)
	if err != nil {
		// gen.Error is three strings and a slice of small structs; this is
		// unreachable. Answering with a hand-written body rather than a
		// half-written one is what keeps "every error has one shape" true even
		// on the impossible branch.
		http.Error(w, `{"code":"internal","message":"Something went wrong.","request_id":""}`,
			http.StatusInternalServerError)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	// An error response is specific to one caller and one moment; a shared
	// cache holding a 429 or a 401 would serve it to somebody else.
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(p.Status)

	// The status line is already written. A failed write means the client has
	// gone; there is nothing to report to and nothing to recover, and the
	// access-log line still records the status.
	_, _ = w.Write(buf)
}

// specCode maps a middleware.Code* constant onto the closed `Error.code` enum
// in openapi.yaml.
//
// The two vocabularies are deliberately not merged. internal/httpapi/middleware
// is a transport-layer package and names transport conditions
// ("payload_too_large", "timeout"); the spec names PRODUCT conditions a client
// branches on, and has no entry for either. Translating at this one boundary
// keeps the middleware reusable and keeps the wire inside the contract.
//
// An unrecognised code becomes `internal` rather than travelling through
// unchanged: a code outside the enum is undecodable by a generated client, so
// the failure would land on the caller instead of here.
func specCode(code string) gen.ErrorCode {
	switch code {
	case middleware.CodeBadRequest, middleware.CodePayloadTooLarge:
		// The spec has no 413 response and no payload_too_large code. A body
		// over the cap is a malformed request, which is what bad_request means.
		return gen.ErrorCodeBadRequest
	case middleware.CodeUnauthorized:
		// The spec spells this `unauthenticated`, which is the accurate word:
		// HTTP's "Unauthorized" has always meant "unauthenticated".
		return gen.ErrorCodeUnauthenticated
	case middleware.CodeForbidden:
		return gen.ErrorCodeForbidden
	case middleware.CodeNotFound:
		return gen.ErrorCodeNotFound
	case middleware.CodeRateLimited:
		return gen.ErrorCodeRateLimited
	case middleware.CodeTimeout, middleware.CodeInternal:
		// A deadline this service failed to meet is this service's failure, and
		// the spec has one code for that.
		return gen.ErrorCodeInternal
	default:
		if c := gen.ErrorCode(code); c.Valid() {
			return c
		}
		return gen.ErrorCodeInternal
	}
}
