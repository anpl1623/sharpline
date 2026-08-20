package httpapi

import (
	"bufio"
	"net/http"
	"strings"
	"testing"
)

// THE TEST THAT MAKES THE SPEC A CONTRACT.
//
// openapi.yaml declares a set of (path, method) pairs. [API.Routes] mounts a set
// of (path, method) pairs. This test asserts they are THE SAME SET in both
// directions:
//
//   - a handler mounted at a path the spec does not declare fails here, because
//     a client generated from the spec cannot reach it and nobody knows it
//     exists;
//   - a path declared in the spec with no handler behind it fails here, because
//     a client generated from the spec WILL reach it and get a 404 from an
//     endpoint its types say exists.
//
// Without this, "spec-first" is an aspiration. `make codegen-check-openapi`
// pins the SHAPES — the generated structs cannot drift from the schemas — and
// this pins the SURFACE. Both are needed: a spec can be perfectly accurate about
// the shape of a response nobody serves.
//
// # Why the spec is parsed by hand
//
// A real YAML parser would mean adding a dependency to a module whose serving
// path deliberately has none (see internal/httpapi/oapi-codegen.yaml on why
// `embedded-spec` is declined for the same reason). The parser below reads only
// the `paths:` block, only two indent levels, and REFUSES TO GUESS: if it finds
// no paths, or a method line it does not recognise, it fails the test rather
// than silently asserting over an empty set. A test that passes because it
// parsed nothing is worse than no test.

func TestRouteTableMatchesSpec(t *testing.T) {
	t.Parallel()

	specPairs := parseSpecOperations(t)
	if len(specPairs) == 0 {
		t.Fatal("parsed zero operations from openapi.yaml: the parser or the spec is broken")
	}

	api := newTestAPI(t)

	routePairs := map[string]bool{}
	for _, r := range api.Routes() {
		// The spec-serving route is the one legitimate exception: it serves the
		// document itself and sits outside /v1, so it is not an operation the
		// document could declare without declaring itself.
		if r.Path == "/openapi.yaml" {
			continue
		}
		path, ok := strings.CutPrefix(r.Path, "/"+APIVersion)
		if !ok {
			t.Fatalf("route %s %s is not under /%s; every product route must be versioned",
				r.Method, r.Path, APIVersion)
		}
		routePairs[r.Method+" "+path] = true
	}

	for pair := range specPairs {
		if !routePairs[pair] {
			t.Errorf("openapi.yaml declares %q but no handler is mounted for it", pair)
		}
	}
	for pair := range routePairs {
		if !specPairs[pair] {
			t.Errorf("a handler is mounted for %q but openapi.yaml does not declare it", pair)
		}
	}
}

// TestSessionlessBuildDropsExactlyTheSessionRoutes.
//
// [APIOptions.Sessions] is the one optional port, so that a deployment without
// an auth adapter still serves the whole read surface instead of going dark. The
// degradation has to be EXACT: the auth and TOTP routes disappear and nothing
// else moves. A build that also dropped the board would be a silent outage, and
// one that kept /auth/login would 500 on a nil port.
func TestSessionlessBuildDropsExactlyTheSessionRoutes(t *testing.T) {
	t.Parallel()

	full := newTestAPI(t)

	d := newDeps()
	d.sessions = nil
	partial, err := NewAPI(APIOptions{
		Catalogue: d.catalogue, Prices: d.prices, Ledger: d.ledger,
		Accounts: d.accounts, Limits: d.limits, Audit: d.audit,
		Betting: d.betting, Wagers: d.wagers, Pricer: d.pricer,
		CashOutQuotes: d.cashOuts, CashOuts: d.cashOuts,
		Logger: d.logger, Now: fixedClock(),
		RequireAuth: []Middleware{func(next http.Handler) http.Handler { return next }},
	})
	if err != nil {
		t.Fatalf("NewAPI without Sessions: %v", err)
	}
	if partial.HasSessions() {
		t.Error("HasSessions reports true with a nil port")
	}

	got := map[string]bool{}
	for _, r := range partial.Routes() {
		got[r.Method+" "+r.Path] = true
	}

	wantGone := map[string]bool{
		"POST /v1/auth/register":        true,
		"POST /v1/auth/login":           true,
		"POST /v1/auth/refresh":         true,
		"POST /v1/auth/logout":          true,
		"POST /v1/account/totp":         true,
		"POST /v1/account/totp/confirm": true,
		"DELETE /v1/account/totp":       true,
	}

	for _, r := range full.Routes() {
		key := r.Method + " " + r.Path
		switch {
		case wantGone[key] && got[key]:
			t.Errorf("%s is still mounted with a nil session port; it would panic", key)
		case !wantGone[key] && !got[key]:
			t.Errorf("%s disappeared with a nil session port; only the auth routes should", key)
		}
	}
	if len(got)+len(wantGone) != len(full.Routes()) {
		t.Errorf("sessionless build has %d routes, full has %d, %d were expected to drop",
			len(got), len(full.Routes()), len(wantGone))
	}
}

// TestEveryRouteIsWellFormed catches the mistakes the set comparison cannot: a
// nil handler, a duplicate registration, an unversioned path.
func TestEveryRouteIsWellFormed(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t)
	seen := map[string]bool{}

	for _, r := range api.Routes() {
		key := r.Method + " " + r.Path
		if seen[key] {
			t.Errorf("route %s is registered twice; net/http would panic at mount", key)
		}
		seen[key] = true

		if r.Handler == nil {
			t.Errorf("route %s has a nil handler", key)
		}
		if !strings.HasPrefix(r.Path, "/") {
			t.Errorf("route %s path must be absolute", key)
		}
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch:
		default:
			t.Errorf("route %s uses an unexpected method", key)
		}
	}
}

// privatePrefixes are the route subtrees that act on the caller's own data and
// therefore MUST carry the authentication middleware. Everything else is the
// public catalogue and must NOT.
//
// It is a list rather than a single prefix because phase 8 added a second
// private subtree: /v1/wagers and /v1/slip are the customer's own tickets and
// the customer's own slip, and they are as private as /v1/account is. Keeping
// them in one list rather than loosening the test is what stops the assertion
// decaying into "some routes have middleware".
var privatePrefixes = []string{"/account", "/wagers", "/slip"}

// TestPrivateRoutesRequireAuthentication is the one that would matter if it ever
// failed.
//
// Every route under a private subtree carries the authentication middleware, and
// every route outside them does not. A private route that lost its middleware
// would still return a body — [API.caller] answers 401 on a missing identity —
// but it would do so AFTER the rate limiter had counted it as anonymous and
// after the handler had been entered, and the next handler written without that
// reflex would leak. So the property is asserted on the ROUTE TABLE, where it is
// decided.
func TestPrivateRoutesRequireAuthentication(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t)
	for _, r := range api.Routes() {
		private := false
		for _, prefix := range privatePrefixes {
			if strings.HasPrefix(r.Path, "/"+APIVersion+prefix) {
				private = true
				break
			}
		}
		switch {
		case private && len(r.Middleware) == 0:
			t.Errorf("%s %s acts on the caller's own data but carries no authentication middleware",
				r.Method, r.Path)
		case !private && len(r.Middleware) > 0:
			// Not a security failure, but it means a public route is paying for
			// authentication it does not use, and it is far more likely to be a
			// copy-paste than a decision.
			t.Errorf("%s %s is public but carries per-route middleware; was that intended?", r.Method, r.Path)
		}
	}
}

// TestBettingRoutesNameNoUser restates, on the route table, the property every
// handler in this package depends on: NOTHING BELOW /v1 TAKES A USER AS A
// PARAMETER.
//
// [API.caller] is the only source of the caller's identity, so "act on behalf of
// somebody else" is not expressible rather than merely forbidden — and that is
// why none of these handlers contains an authorization check. The one identifier
// that does belong to somebody is {wagerId}, and the read that resolves it takes
// the caller's id and answers 404 for a ticket that is not theirs.
//
// A path wildcard named for a user would silently reopen the question, so it
// fails here.
func TestBettingRoutesNameNoUser(t *testing.T) {
	t.Parallel()

	forbidden := []string{"{userId}", "{user}", "{accountId}", "{customerId}"}
	api := newTestAPI(t)
	for _, r := range api.Routes() {
		for _, wildcard := range forbidden {
			if strings.Contains(r.Path, wildcard) {
				t.Errorf("%s %s names a user in its path; identity comes from the token and nowhere else",
					r.Method, r.Path)
			}
		}
	}
}

// TestCashOutRoutesFollowTheirPorts asserts the degradation is EXACT.
//
// The two cash-out capabilities are independently optional: pricing an early
// close needs devigged reference prices, taking one needs the settlement write
// path, and a deployment can plausibly have either without the other. A build
// missing one must drop exactly that route and nothing else — a build that
// dropped the wager history along with the cash-out would be a silent outage,
// and one that kept the take without an executor would panic on a nil port.
func TestCashOutRoutesFollowTheirPorts(t *testing.T) {
	t.Parallel()

	const (
		quotePath = "GET /" + APIVersion + "/wagers/{wagerId}/cashout"
		takePath  = "POST /" + APIVersion + "/wagers/{wagerId}/cashout"
	)

	build := func(t *testing.T, quotes CashOutQuotes, takes CashOuts) map[string]bool {
		t.Helper()
		d := newDeps()
		api, err := NewAPI(APIOptions{
			Catalogue: d.catalogue, Prices: d.prices, Ledger: d.ledger,
			Accounts: d.accounts, Limits: d.limits, Audit: d.audit,
			Sessions: d.sessions,
			Betting:  d.betting, Wagers: d.wagers, Pricer: d.pricer,
			CashOutQuotes: quotes, CashOuts: takes,
			Logger: d.logger, Now: fixedClock(),
			RequireAuth: []Middleware{func(next http.Handler) http.Handler { return next }},
		})
		if err != nil {
			t.Fatalf("NewAPI: %v", err)
		}
		got := map[string]bool{}
		for _, r := range api.Routes() {
			got[r.Method+" "+r.Path] = true
		}
		return got
	}

	full := newTestAPI(t)
	fullCount := len(full.Routes())
	if !full.HasCashOutQuotes() || !full.HasTakeCashOut() {
		t.Fatal("the full build reports a cash-out port unwired; every later count here would be wrong")
	}

	cases := []struct {
		name       string
		quotes     CashOutQuotes
		takes      CashOuts
		wantQuote  bool
		wantTake   bool
		wantRoutes int
	}{
		{"both wired", &fakeCashOuts{}, &fakeCashOuts{}, true, true, fullCount},
		{"quote only", &fakeCashOuts{}, nil, true, false, fullCount - 1},
		{"take only", nil, &fakeCashOuts{}, false, true, fullCount - 1},
		{"neither", nil, nil, false, false, fullCount - 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := build(t, tc.quotes, tc.takes)
			if got[quotePath] != tc.wantQuote {
				t.Errorf("%s mounted = %v, want %v", quotePath, got[quotePath], tc.wantQuote)
			}
			if got[takePath] != tc.wantTake {
				t.Errorf("%s mounted = %v, want %v", takePath, got[takePath], tc.wantTake)
			}
			if len(got) != tc.wantRoutes {
				t.Errorf("route count = %d, want %d; only the cash-out routes may move",
					len(got), tc.wantRoutes)
			}
		})
	}
}

// TestAuthRoutesAreUnauthenticated states the converse explicitly: the endpoints
// that MINT a credential cannot require one, or authentication is circular and
// nobody can ever log in.
func TestAuthRoutesAreUnauthenticated(t *testing.T) {
	t.Parallel()

	api := newTestAPI(t)
	for _, r := range api.Routes() {
		if strings.HasPrefix(r.Path, "/"+APIVersion+"/auth") && len(r.Middleware) > 0 {
			t.Errorf("%s %s requires authentication, which makes it unreachable", r.Method, r.Path)
		}
	}
}

// parseSpecOperations extracts "METHOD /path" pairs from the embedded document.
//
// Deliberately strict. It tracks exactly two nesting levels below `paths:` and
// treats anything unexpected as a parse failure, so a spec restructured in a way
// this parser does not understand fails the test loudly instead of quietly
// asserting over a smaller set than it should.
func parseSpecOperations(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := SpecBytes()
	if err != nil {
		t.Fatalf("read embedded spec: %v", err)
	}

	const (
		pathIndent   = 2 // "  /board:"
		methodIndent = 4 // "    get:"
	)
	methods := map[string]string{
		"get":    http.MethodGet,
		"post":   http.MethodPost,
		"put":    http.MethodPut,
		"patch":  http.MethodPatch,
		"delete": http.MethodDelete,
	}

	out := map[string]bool{}
	inPaths := false
	current := ""

	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}

		indent := len(line) - len(strings.TrimLeft(line, " "))
		trimmed := strings.TrimSpace(line)

		// A top-level key ends the paths block.
		if indent == 0 {
			inPaths = trimmed == "paths:"
			current = ""
			continue
		}
		if !inPaths {
			continue
		}

		switch indent {
		case pathIndent:
			if !strings.HasPrefix(trimmed, "/") || !strings.HasSuffix(trimmed, ":") {
				t.Fatalf("unexpected line at path level in openapi.yaml: %q", line)
			}
			current = strings.TrimSuffix(trimmed, ":")
		case methodIndent:
			key := strings.TrimSuffix(trimmed, ":")
			method, ok := methods[key]
			if !ok {
				// `parameters:` is a legal sibling of the operations and is not
				// one; anything else at this level is a spec shape this parser
				// does not model and must not be silently skipped.
				if key == "parameters" || key == "summary" || key == "description" {
					continue
				}
				t.Fatalf("unrecognised key at operation level in openapi.yaml: %q", line)
			}
			if current == "" {
				t.Fatalf("operation %q appears before any path", line)
			}
			out[method+" "+current] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan spec: %v", err)
	}
	return out
}

// TestSpecIsServedVerbatim asserts the served document is the embedded one, byte
// for byte. The point of serving the spec is that a client can trust it is this
// build's contract; a transformed copy would defeat that.
func TestSpecIsServedVerbatim(t *testing.T) {
	t.Parallel()

	want, err := SpecBytes()
	if err != nil {
		t.Fatalf("SpecBytes: %v", err)
	}
	api := newTestAPI(t)

	rec, req := newRequest(http.MethodGet, "/openapi.yaml", nil)
	api.handleSpec(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != string(want) {
		t.Errorf("served spec differs from the embedded document (%d vs %d bytes)", len(got), len(want))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
}
