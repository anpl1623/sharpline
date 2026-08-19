package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/httpapi/gen"
	"github.com/anpl1623/sharpline/internal/httpapi/middleware"
)

func okHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	})
}

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// do drives the composed handler. RemoteAddr is set because a real listener
// always sets one and the middleware chain reads it.
func do(s *Server, method, target string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.RemoteAddr = "172.18.0.7:54321"
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) gen.Error {
	t.Helper()
	var body gen.Error
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body.String(), err)
	}
	return body
}

func TestNewRejectsUnusableOptions(t *testing.T) {
	t.Parallel()

	cases := map[string]Options{
		"prefix without a leading slash": {PublicPrefix: "api"},
		"route without a method": {
			Routes: []Route{{Path: "/v1/x", Handler: okHandler("")}},
		},
		"route path without a leading slash": {
			Routes: []Route{{Method: http.MethodGet, Path: "v1/x", Handler: okHandler("")}},
		},
		"route without a handler": {
			Routes: []Route{{Method: http.MethodGet, Path: "/v1/x"}},
		},
		"duplicate route": {
			Routes: []Route{
				{Method: http.MethodGet, Path: "/v1/x", Handler: okHandler("")},
				{Method: http.MethodGet, Path: "/v1/x", Handler: okHandler("")},
			},
		},
		"nil route set": {RouteSets: []RouteSet{nil}},
		"nil route middleware": {
			Routes: []Route{{
				Method: http.MethodGet, Path: "/v1/x", Handler: okHandler(""),
				Middleware: []Middleware{nil},
			}},
		},
	}

	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(opts); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("want ErrInvalidOptions, got %v", err)
			}
		})
	}
}

// routeSetFunc adapts a function to RouteSet, the way a handler struct in a
// sibling file does.
type routeSetFunc func() []Route

func (f routeSetFunc) Routes() []Route { return f() }

func TestRoutesAndRouteSetsAreBothMounted(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, Options{
		Routes: []Route{{Method: http.MethodGet, Path: "/v1/sports", Handler: okHandler("sports")}},
		RouteSets: []RouteSet{routeSetFunc(func() []Route {
			return []Route{{Method: http.MethodPost, Path: "/v1/auth/login", Handler: okHandler("login")}}
		})},
	})

	got := strings.Join(s.Patterns(), "|")
	// Patterns is sorted, so this is the whole table in one comparison.
	if want := "GET /api/v1/sports|POST /api/v1/auth/login"; got != want {
		t.Fatalf("Patterns() = %q, want %q", got, want)
	}

	if w := do(s, http.MethodGet, "/api/v1/sports"); w.Body.String() != "sports" {
		t.Fatalf("GET /api/v1/sports = %q", w.Body.String())
	}
	if w := do(s, http.MethodPost, "/api/v1/auth/login"); w.Body.String() != "login" {
		t.Fatalf("POST /api/v1/auth/login = %q", w.Body.String())
	}
}

// A path no route claims must answer with the spec's Error shape, not
// net/http's plain-text 404. The keys asserted here are the ones
// `additionalProperties: false` in openapi.yaml pins.
func TestUnmatchedPathAnswersSpecShapedNotFound(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, Options{
		Routes: []Route{{Method: http.MethodGet, Path: "/v1/sports", Handler: okHandler("")}},
	})

	w := do(s, http.MethodGet, "/api/v1/nope")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q", ct)
	}
	if body := decodeError(t, w); body.Code != gen.ErrorCodeNotFound {
		t.Fatalf("code = %q, want %q", body.Code, gen.ErrorCodeNotFound)
	}

	// The envelope is flat: exactly the spec's keys and no wrapper object. A
	// nested {"error":{...}} would decode into a zero-valued gen.Error above
	// without failing, so the raw keys are checked directly.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"code", "message", "request_id"} {
		if _, ok := raw[k]; !ok {
			t.Fatalf("body %s is missing key %q", w.Body.String(), k)
		}
	}
	if len(raw) != 3 {
		t.Fatalf("body has %d keys, want exactly code/message/request_id: %s", len(raw), w.Body.String())
	}
}

// The reason [Server.handleUnmatched] exists at all: with a "/api/" subtree
// pattern registered, net/http's own 405 never fires, and a GET to a POST-only
// path would be answered 404 — telling a client its request was wrong in the
// wrong way.
func TestWrongMethodIsMethodNotAllowedWithAllow(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, Options{
		Routes: []Route{
			{Method: http.MethodPost, Path: "/v1/auth/login", Handler: okHandler("")},
			{Method: http.MethodGet, Path: "/v1/account/limits", Handler: okHandler("")},
			{Method: http.MethodPost, Path: "/v1/account/limits", Handler: okHandler("")},
		},
	})

	w := do(s, http.MethodGet, "/api/v1/auth/login")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
	// openapi.yaml's Error.code enum has no method_not_allowed, so the code is
	// the coarser bad_request and the Allow header carries the precise answer.
	if body := decodeError(t, w); body.Code != gen.ErrorCodeBadRequest {
		t.Fatalf("code = %q, want %q", body.Code, gen.ErrorCodeBadRequest)
	}

	// Two methods on one path: Allow lists both, plus the HEAD net/http serves
	// for free off the GET.
	w = do(s, http.MethodDelete, "/api/v1/account/limits")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if allow := w.Header().Get("Allow"); allow != "GET, HEAD, POST" {
		t.Fatalf("Allow = %q, want %q", allow, "GET, HEAD, POST")
	}
}

func TestGetRouteAlsoServesHead(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, Options{
		Routes: []Route{{Method: http.MethodGet, Path: "/v1/sports", Handler: okHandler("sports")}},
	})

	if w := do(s, http.MethodHead, "/api/v1/sports"); w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", w.Code)
	}
}

func TestGlobalMiddlewareWrapsUnmatchedRequestsAndRouteMiddlewareDoesNot(t *testing.T) {
	t.Parallel()

	var global, perRoute int
	s := newTestServer(t, Options{
		Middleware: []Middleware{func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				global++
				next.ServeHTTP(w, r)
			})
		}},
		Routes: []Route{{
			Method: http.MethodGet, Path: "/v1/x", Handler: okHandler(""),
			Middleware: []Middleware{func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					perRoute++
					next.ServeHTTP(w, r)
				})
			}},
		}},
	})

	do(s, http.MethodGet, "/api/v1/x")
	do(s, http.MethodGet, "/api/v1/missing")

	// The per-IP limiter has to see the scanner hammering paths that do not
	// exist; a per-route limiter by definition cannot.
	if global != 2 {
		t.Fatalf("global middleware ran %d times, want 2 (including the unmatched request)", global)
	}
	if perRoute != 1 {
		t.Fatalf("route middleware ran %d times, want 1", perRoute)
	}
}

func TestRouteMiddlewareRunsOutermostFirst(t *testing.T) {
	t.Parallel()

	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}

	s := newTestServer(t, Options{
		Routes: []Route{{
			Method: http.MethodGet, Path: "/v1/x",
			Middleware: []Middleware{mark("first"), mark("second")},
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				order = append(order, "handler")
			}),
		}},
	})

	do(s, http.MethodGet, "/api/v1/x")
	if got, want := strings.Join(order, ","), "first,second,handler"; got != want {
		t.Fatalf("order = %q, want %q", got, want)
	}
}

// Mount must register exactly one subtree pattern, so the probes the
// operational listener already mirrors beneath the prefix keep winning.
func TestMountRegistersOneSubtreePattern(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	var probed bool
	mux.HandleFunc("GET /api/healthz", func(w http.ResponseWriter, _ *http.Request) { probed = true })

	s := newTestServer(t, Options{
		Routes: []Route{{Method: http.MethodGet, Path: "/v1/sports", Handler: okHandler("sports")}},
	})
	s.Mount(mux)

	mux.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/healthz", nil))
	if !probed {
		t.Fatal("mounting the API shadowed the operational probe at /api/healthz")
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sports", nil)
	r.RemoteAddr = "172.18.0.7:1"
	mux.ServeHTTP(w, r)
	if w.Body.String() != "sports" {
		t.Fatalf("mounted route body = %q", w.Body.String())
	}
}

func TestPrefixIsConfigurableAndNormalised(t *testing.T) {
	t.Parallel()

	s := newTestServer(t, Options{
		PublicPrefix: "/gateway/",
		Routes:       []Route{{Method: http.MethodGet, Path: "/v1/x", Handler: okHandler("x")}},
	})
	if s.Prefix() != "/gateway" {
		t.Fatalf("Prefix() = %q", s.Prefix())
	}
	if w := do(s, http.MethodGet, "/gateway/v1/x"); w.Body.String() != "x" {
		t.Fatalf("body = %q", w.Body.String())
	}
}

func TestInjectedErrorWriterIsUsedForFallbacks(t *testing.T) {
	t.Parallel()

	var seen []middleware.Problem
	s := newTestServer(t, Options{
		ErrorWriter: func(w http.ResponseWriter, _ *http.Request, p middleware.Problem) {
			seen = append(seen, p)
			w.WriteHeader(p.Status)
		},
		Routes: []Route{{Method: http.MethodPost, Path: "/v1/x", Handler: okHandler("")}},
	})

	do(s, http.MethodGet, "/api/v1/x")
	do(s, http.MethodGet, "/api/v1/nothing-here")

	if len(seen) != 2 {
		t.Fatalf("error writer called %d times, want 2", len(seen))
	}
	if seen[0].Status != http.StatusMethodNotAllowed || seen[1].Status != http.StatusNotFound {
		t.Fatalf("problems = %+v", seen)
	}
}

// Every code the middleware chain can emit must land inside openapi.yaml's
// closed Error.code enum. A code outside it is undecodable by a client
// generated from the spec, so the failure would land on the caller.
func TestSpecCodeMapsEveryMiddlewareCodeIntoTheEnum(t *testing.T) {
	t.Parallel()

	cases := map[string]gen.ErrorCode{
		middleware.CodeBadRequest:      gen.ErrorCodeBadRequest,
		middleware.CodePayloadTooLarge: gen.ErrorCodeBadRequest,
		middleware.CodeUnauthorized:    gen.ErrorCodeUnauthenticated,
		middleware.CodeForbidden:       gen.ErrorCodeForbidden,
		middleware.CodeNotFound:        gen.ErrorCodeNotFound,
		middleware.CodeRateLimited:     gen.ErrorCodeRateLimited,
		middleware.CodeTimeout:         gen.ErrorCodeInternal,
		middleware.CodeInternal:        gen.ErrorCodeInternal,

		// A handler that already speaks the spec's vocabulary passes through.
		"invalid_credentials": gen.ErrorCodeInvalidCredentials,
		"totp_required":       gen.ErrorCodeTotpRequired,

		// Anything else is reported as internal rather than travelling
		// through as a code the spec does not list.
		"something_invented": gen.ErrorCodeInternal,
		"":                   gen.ErrorCodeInternal,
	}

	for in, want := range cases {
		if got := specCode(in); got != want {
			t.Errorf("specCode(%q) = %q, want %q", in, got, want)
		}
		if !specCode(in).Valid() {
			t.Errorf("specCode(%q) produced a value outside the spec enum", in)
		}
	}
}

func TestWriteAPIErrorSetsNoStoreAndNosniff(t *testing.T) {
	t.Parallel()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)
	WriteAPIError(w, r, ProblemNotFound)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache-control = %q; a shared cache holding a 401 would serve it to somebody else", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("nosniff = %q", got)
	}
}
