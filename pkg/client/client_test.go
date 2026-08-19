package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// noSleep makes backoff deterministic and instantaneous: full jitter with a
// zero draw is a zero delay, so the retry logic is exercised without the suite
// waiting for it.
func noSleep() RetryPolicy {
	return RetryPolicy{MaxAttempts: 3, Jitter: func() float64 { return 0 }}
}

func newTestClient(t *testing.T, h http.Handler, mutate ...func(*Options)) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	opts := Options{BaseURL: srv.URL, HTTPClient: srv.Client(), Retry: noSleep()}
	for _, m := range mutate {
		m(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, srv
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func writeAPIError(t *testing.T, w http.ResponseWriter, status int, code ErrorCode, msg string) {
	t.Helper()
	writeJSON(t, w, status, Error{Code: code, Message: msg, RequestId: "req_test"})
}

// -----------------------------------------------------------------------------
// construction
// -----------------------------------------------------------------------------

func TestNewValidatesOptions(t *testing.T) {
	t.Parallel()

	for name, base := range map[string]string{
		"empty":            "",
		"no scheme":        "sharpline.example",
		"wrong scheme":     "ftp://sharpline.example",
		"no host":          "https://",
		"carries a query":  "https://sharpline.example/api/v1?x=1",
		"carries fragment": "https://sharpline.example/api/v1#x",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(Options{BaseURL: base}); !errors.Is(err, ErrInvalidOptions) {
				t.Fatalf("New(%q) = %v, want ErrInvalidOptions", base, err)
			}
		})
	}
}

// A caller should be able to pass the site root and get the documented API
// path, or pass an explicit path and have it respected — and neither should
// produce a doubled prefix.
func TestNewResolvesTheBasePath(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"https://sharpline.example":            "https://sharpline.example/api/v1",
		"https://sharpline.example/":           "https://sharpline.example/api/v1",
		"https://sharpline.example/api/v1":     "https://sharpline.example/api/v1",
		"https://sharpline.example/api/v1/":    "https://sharpline.example/api/v1",
		"https://sharpline.example/gw/api/v2":  "https://sharpline.example/gw/api/v2",
		"http://localhost:8080":                "http://localhost:8080/api/v1",
		"https://sharpline.example:8443/proxy": "https://sharpline.example:8443/proxy",
	}
	for in, want := range cases {
		c, err := New(Options{BaseURL: in})
		if err != nil {
			t.Fatalf("New(%q): %v", in, err)
		}
		if got := c.BaseURL(); got != want {
			t.Errorf("New(%q).BaseURL() = %q, want %q", in, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// requests
// -----------------------------------------------------------------------------

func TestPublicCallSendsNoAuthorizationHeader(t *testing.T) {
	t.Parallel()

	var authz string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authz = r.Header.Get("Authorization")
		writeJSON(t, w, http.StatusOK, SportPage{Data: []Sport{}})
	}))

	if _, err := c.Sports(context.Background()); err != nil {
		t.Fatalf("Sports: %v", err)
	}
	if authz != "" {
		t.Fatalf("Authorization = %q on a public call", authz)
	}
}

func TestAuthenticatedCallWithoutASessionFailsBeforeSending(t *testing.T) {
	t.Parallel()

	var requests int
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		writeJSON(t, w, http.StatusOK, Account{})
	}))

	_, err := c.Account(context.Background())
	if !errors.Is(err, ErrNoSession) {
		t.Fatalf("err = %v, want ErrNoSession", err)
	}
	// The point of failing early: an unauthenticated request to an
	// authenticated endpoint is a wasted round trip AND a 401 in the server's
	// logs that looks like an attack.
	if requests != 0 {
		t.Fatalf("%d requests were sent without a credential", requests)
	}
}

func TestPathAndQueryEncoding(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotQuery url.Values
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		writeJSON(t, w, http.StatusOK, BoardPage{})
	}))

	limit := int32(25)
	cursor := "Y3Vyc29y"
	format := OddsFormatAmerican
	books := BookFilter{"pinnacle", "sharpline"}
	when := time.Date(2026, 8, 19, 12, 0, 0, 0, time.FixedZone("x", 3600))

	_, err := c.LeagueBoard(context.Background(), "nfl", GetLeagueBoardParams{
		StartingBefore: &when,
		Limit:          &limit,
		Cursor:         &cursor,
		OddsFormat:     &format,
		Book:           &books,
	})
	if err != nil {
		t.Fatalf("LeagueBoard: %v", err)
	}

	if want := "/api/v1/leagues/nfl/board"; gotPath != want {
		t.Fatalf("path = %q, want %q", gotPath, want)
	}
	// Repeated, not comma-joined: a slug cannot contain a comma, so a joined
	// value would be rejected as one unknown book rather than filtering by two.
	if got := gotQuery["book"]; len(got) != 2 || got[0] != "pinnacle" || got[1] != "sharpline" {
		t.Fatalf("book = %v, want two repeated values", got)
	}
	if got := gotQuery.Get("limit"); got != "25" {
		t.Fatalf("limit = %q", got)
	}
	if got := gotQuery.Get("cursor"); got != cursor {
		t.Fatalf("cursor = %q", got)
	}
	// UTC, so two clients asking for the same instant send the same string.
	if got := gotQuery.Get("starting_before"); !strings.HasSuffix(got, "Z") {
		t.Fatalf("starting_before = %q, want a UTC instant", got)
	}
}

// A nil optional parameter must produce NO query parameter. `?cursor=` is a
// present-but-empty cursor, which the server rejects as undecodable, where an
// absent cursor means "the first page".
func TestNilOptionalParametersAreOmitted(t *testing.T) {
	t.Parallel()

	var raw string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.RawQuery
		writeJSON(t, w, http.StatusOK, BoardPage{})
	}))

	if _, err := c.Board(context.Background(), GetBoardParams{}); err != nil {
		t.Fatalf("Board: %v", err)
	}
	if raw != "" {
		t.Fatalf("query = %q, want empty", raw)
	}
}

func TestNoContentResponseIsNotDecoded(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), func(o *Options) { o.Tokens = StaticToken("t") })

	if err := c.RemoveTOTP(context.Background(), "123456"); err != nil {
		t.Fatalf("RemoveTOTP: %v", err)
	}
}

// The server may add a field to a response at any time. A client that refused
// the whole payload over one it did not recognise would break every existing
// caller on a purely additive change.
func TestUnknownResponseFieldsAreTolerated(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"s1","slug":"nfl","name":"Football","invented_later":true}],` +
			`"page":{"limit":50,"has_more":false}}`))
	}))

	page, err := c.Sports(context.Background())
	if err != nil {
		t.Fatalf("Sports: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].Slug != "nfl" {
		t.Fatalf("page = %+v", page)
	}
}

// -----------------------------------------------------------------------------
// errors
// -----------------------------------------------------------------------------

func TestAPIErrorDecodesTheEnvelope(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusUnprocessableEntity, Error{
			Code:      ErrorCodeUnprocessable,
			Message:   "The window would exceed max_points.",
			RequestId: "req_abc123",
			InvalidParams: &[]InvalidParam{
				{Name: "resolution", Reason: "too fine for this window"},
			},
		})
	}))

	from := time.Now().Add(-time.Hour)
	_, err := c.SelectionHistory(context.Background(), "sel_1", GetSelectionHistoryParams{
		Book: "pinnacle", From: from,
	})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d", apiErr.Status)
	}
	if apiErr.RequestID != "req_abc123" {
		t.Errorf("request_id = %q; it is the only handle on the server log line", apiErr.RequestID)
	}
	if len(apiErr.InvalidParams) != 1 || apiErr.InvalidParams[0].Name != "resolution" {
		t.Errorf("invalid_params = %+v", apiErr.InvalidParams)
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("422 does not match ErrInvalidRequest")
	}
	// The op label is present and is not a URL — a URL would put whatever the
	// caller appended to a query into the caller's logs.
	if !strings.Contains(apiErr.Error(), "GET /selections/{selectionId}/history") {
		t.Errorf("Error() = %q, want the operation label", apiErr.Error())
	}
	if strings.Contains(apiErr.Error(), "http://") {
		t.Errorf("Error() leaks a URL: %q", apiErr.Error())
	}
}

func TestSentinelsMatchOnCodeAndOnStatusClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status  int
		code    ErrorCode
		match   []error
		noMatch []error
	}{
		{http.StatusUnauthorized, ErrorCodeInvalidCredentials,
			[]error{ErrUnauthenticated, ErrInvalidCredentials},
			[]error{ErrTOTPRequired, ErrForbidden}},
		{http.StatusUnauthorized, ErrorCodeTotpRequired,
			[]error{ErrUnauthenticated, ErrTOTPRequired},
			[]error{ErrInvalidCredentials}},
		{http.StatusForbidden, ErrorCodeAccountNotActive,
			[]error{ErrForbidden, ErrAccountNotActive},
			[]error{ErrUnauthenticated}},
		{http.StatusConflict, ErrorCodeAlreadyExists,
			[]error{ErrConflict, ErrAlreadyExists}, nil},
		{http.StatusBadRequest, ErrorCodeInvalidCursor,
			[]error{ErrInvalidRequest, ErrInvalidCursor}, nil},
		{http.StatusTooManyRequests, ErrorCodeRateLimited,
			[]error{ErrRateLimited}, []error{ErrServer}},
		{http.StatusInternalServerError, ErrorCodeInternal,
			[]error{ErrServer}, []error{ErrInvalidRequest}},
		{http.StatusNotFound, ErrorCodeNotFound,
			[]error{ErrNotFound}, []error{ErrForbidden}},
	}

	for _, tc := range cases {
		err := error(asAPIError("GET /x", tc.status, &Error{Code: tc.code}, 0))
		for _, want := range tc.match {
			if !errors.Is(err, want) {
				t.Errorf("%d/%s does not match %v", tc.status, tc.code, want)
			}
		}
		for _, unwanted := range tc.noMatch {
			if errors.Is(err, unwanted) {
				t.Errorf("%d/%s wrongly matches %v", tc.status, tc.code, unwanted)
			}
		}
	}
}

// A proxy 502 has no envelope to decode. The client must still produce a
// usable error rather than inventing a code the server never sent.
func TestNonJSONErrorBodyStillProducesAnAPIError(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>bad gateway</html>"))
	}), func(o *Options) { o.Retry = RetryPolicy{MaxAttempts: 1} })

	_, err := c.Sports(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Code != "" {
		t.Errorf("code = %q, want empty; nothing was decoded", apiErr.Code)
	}
	if apiErr.Message != http.StatusText(http.StatusBadGateway) {
		t.Errorf("message = %q", apiErr.Message)
	}
	if !errors.Is(err, ErrServer) {
		t.Errorf("502 does not match ErrServer")
	}
}

// -----------------------------------------------------------------------------
// retries
// -----------------------------------------------------------------------------

func TestSafeRequestIsRetried(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeJSON(t, w, http.StatusOK, SportPage{Data: []Sport{{Slug: "nfl"}}})
	}))

	page, err := c.Sports(context.Background())
	if err != nil {
		t.Fatalf("Sports: %v", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	if len(page.Data) != 1 {
		t.Fatalf("page = %+v", page)
	}
}

// The single most important retry decision in this package. A retry of a
// mutating request whose response was lost on the wire applies it twice; for
// POST /auth/refresh that means presenting a redeemed refresh token, which the
// server cannot distinguish from theft and answers by revoking the whole login
// family.
func TestMutatingRequestIsNeverRetried(t *testing.T) {
	t.Parallel()

	for name, fn := range map[string]func(*Client) error{
		"login": func(c *Client) error {
			_, err := c.Login(context.Background(), Credentials{Email: "a@b.test", Password: "x"})
			return err
		},
		"refresh": func(c *Client) error {
			_, err := c.refreshSession(context.Background(), "rt")
			return err
		},
		"set limit": func(c *Client) error {
			_, err := c.SetLimit(context.Background(), SetLimitRequest{Kind: LimitKindLoss, Period: LimitPeriodDay})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var attempts atomic.Int32
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}), func(o *Options) { o.Tokens = StaticToken("t") })

			if err := fn(c); err == nil {
				t.Fatal("want an error")
			}
			if got := attempts.Load(); got != 1 {
				t.Fatalf("attempts = %d, want exactly 1", got)
			}
		})
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	t.Parallel()

	for name, header := range map[string]string{
		"seconds": "2",
		"date":    time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := http.Header{}
			h.Set("Retry-After", header)
			got := retryAfter(h)
			if got <= 0 || got > 3*time.Second {
				t.Fatalf("retryAfter(%q) = %v", header, got)
			}
		})
	}

	if got := retryAfter(http.Header{}); got != 0 {
		t.Fatalf("absent Retry-After = %v, want 0", got)
	}
	// A date in the past means "now", not a negative sleep.
	h := http.Header{}
	h.Set("Retry-After", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
	if got := retryAfter(h); got != 0 {
		t.Fatalf("past Retry-After = %v, want 0", got)
	}
}

func TestRetryStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := c.Sports(ctx); err == nil {
		t.Fatal("want an error")
	}
	// The 30-second Retry-After must not be slept through: the caller's
	// deadline wins, which is what makes ctx meaningful.
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestFullJitterBackoffStaysWithinItsCap(t *testing.T) {
	t.Parallel()

	p := RetryPolicy{BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second, Jitter: func() float64 { return 1 }}
	if got, want := p.delay(1), 100*time.Millisecond; got != want {
		t.Errorf("delay(1) = %v, want %v", got, want)
	}
	if got, want := p.delay(2), 200*time.Millisecond; got != want {
		t.Errorf("delay(2) = %v, want %v", got, want)
	}
	// Capped, and no overflow at a large attempt count.
	for _, attempt := range []int{5, 20, 62, 64} {
		if got := p.delay(attempt); got != time.Second {
			t.Errorf("delay(%d) = %v, want the cap", attempt, got)
		}
	}
	// A zero draw is a zero delay: full jitter is uniform over [0, cap).
	zero := RetryPolicy{Jitter: func() float64 { return 0 }}
	if got := zero.delay(3); got != 0 {
		t.Errorf("zero jitter = %v", got)
	}
}

// -----------------------------------------------------------------------------
// sessions and rotation
// -----------------------------------------------------------------------------

// sessionServer is a small stand-in for the auth surface: it rotates refresh
// tokens the way the real server does and counts how often it was asked to.
type sessionServer struct {
	mu           sync.Mutex
	refreshCalls int
	seen         []string
	accessTTL    int32
	// unauthorizedUntil makes /account answer 401 until this many refreshes
	// have happened.
	unauthorizedUntil int
	accountCalls      atomic.Int32
}

func (s *sessionServer) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusOK, s.session("access-0", "refresh-0"))
	})

	mux.HandleFunc("POST /api/v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		var req RefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		// A token presented twice is reuse. The real server revokes the whole
		// family; this one just refuses, which is enough to fail a test that
		// double-redeems.
		for _, prior := range s.seen {
			if prior == req.RefreshToken {
				s.mu.Unlock()
				writeAPIError(t, w, http.StatusUnauthorized, ErrorCodeUnauthenticated, "token reuse detected")
				return
			}
		}
		s.seen = append(s.seen, req.RefreshToken)
		s.refreshCalls++
		n := s.refreshCalls
		s.mu.Unlock()

		writeJSON(t, w, http.StatusOK,
			s.session(fmt.Sprintf("access-%d", n), fmt.Sprintf("refresh-%d", n)))
	})

	mux.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/v1/account", func(w http.ResponseWriter, r *http.Request) {
		s.accountCalls.Add(1)
		s.mu.Lock()
		stale := s.refreshCalls < s.unauthorizedUntil
		s.mu.Unlock()
		if stale {
			writeAPIError(t, w, http.StatusUnauthorized, ErrorCodeUnauthenticated, "expired")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			writeAPIError(t, w, http.StatusUnauthorized, ErrorCodeUnauthenticated, "no token")
			return
		}
		writeJSON(t, w, http.StatusOK, Account{Id: "usr_1", Email: "sharp@example.test", Status: AccountStatusActive})
	})

	return mux
}

func (s *sessionServer) session(access, refresh string) SessionResponse {
	ttl := s.accessTTL
	if ttl == 0 {
		ttl = 900
	}
	return SessionResponse{
		AccessToken:      access,
		TokenType:        SessionResponseTokenTypeBearer,
		ExpiresIn:        ttl,
		RefreshToken:     refresh,
		RefreshExpiresAt: time.Now().Add(24 * time.Hour),
		Account:          Account{Id: "usr_1", Email: "sharp@example.test", Status: AccountStatusActive},
	}
}

// THE test this package exists for. When N goroutines share a session and the
// access token dies, a naive client redeems the refresh token N times. The
// second redemption presents an already-redeemed token, which the server cannot
// distinguish from theft, so it revokes the whole family and logs the user out
// everywhere.
func TestConcurrentUnauthorizedCallsRefreshExactlyOnce(t *testing.T) {
	t.Parallel()

	backend := &sessionServer{unauthorizedUntil: 1}
	c, _ := newTestClient(t, backend.handler(t))

	sess, err := c.Login(context.Background(), Credentials{Email: "sharp@example.test", Password: "correct horse battery"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	auth := c.WithSession(sess)

	const goroutines = 24
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			_, errs[i] = auth.Account(context.Background())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	backend.mu.Lock()
	calls := backend.refreshCalls
	backend.mu.Unlock()
	if calls != 1 {
		t.Fatalf("refresh was redeemed %d times, want exactly 1", calls)
	}
}

// The same collapse must happen on the PROACTIVE path, where the client
// refreshes before sending because the token is inside the skew of its expiry.
func TestConcurrentProactiveRefreshesCollapseToOne(t *testing.T) {
	t.Parallel()

	backend := &sessionServer{}
	c, _ := newTestClient(t, backend.handler(t))

	sess, err := c.Login(context.Background(), Credentials{Email: "a@b.test", Password: "x"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	// Move the session's clock to just inside the 900-second token's skew
	// window rather than sleeping. Deterministic, and it exercises the exact
	// branch: the login token is stale, the refreshed one is not.
	offset := 890 * time.Second
	sess.mu.Lock()
	sess.now = func() time.Time { return time.Now().Add(offset) }
	sess.mu.Unlock()

	auth := c.WithSession(sess)

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := range goroutines {
		go func() {
			defer wg.Done()
			_, errs[i] = auth.Account(context.Background())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	backend.mu.Lock()
	calls := backend.refreshCalls
	backend.mu.Unlock()
	// One refresh serves all sixteen. More than one would mean an
	// already-redeemed token was presented, which the server treats as theft.
	if calls != 1 {
		t.Fatalf("refresh was redeemed %d times, want exactly 1", calls)
	}
}

// The regression this guards: with a fixed one-minute skew, a deployment whose
// access-token lifetime is shorter than that made every token stale on arrival,
// so the client refreshed on EVERY call. Clamping the skew to half the lifetime
// is what gives a short-lived token a window in which it is actually used.
func TestShortLivedTokensDoNotCauseARefreshPerCall(t *testing.T) {
	t.Parallel()

	backend := &sessionServer{accessTTL: 2}
	c, _ := newTestClient(t, backend.handler(t))

	sess, err := c.Login(context.Background(), Credentials{Email: "a@b.test", Password: "x"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	auth := c.WithSession(sess)

	for i := range 8 {
		if _, err := auth.Account(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	backend.mu.Lock()
	calls := backend.refreshCalls
	backend.mu.Unlock()
	if calls != 0 {
		t.Fatalf("eight calls inside a 2s token's life caused %d refreshes, want 0", calls)
	}
}

func TestEffectiveSkewIsClampedToHalfTheLifetime(t *testing.T) {
	t.Parallel()

	cases := map[time.Duration]time.Duration{
		0:                0,
		-time.Second:     0,
		2 * time.Second:  time.Second,
		time.Minute:      30 * time.Second,
		2 * time.Minute:  time.Minute,
		15 * time.Minute: time.Minute,
	}
	for lifetime, want := range cases {
		if got := effectiveSkew(lifetime); got != want {
			t.Errorf("effectiveSkew(%v) = %v, want %v", lifetime, got, want)
		}
	}
}

func TestOnRotateReceivesTheNewRefreshToken(t *testing.T) {
	t.Parallel()

	backend := &sessionServer{unauthorizedUntil: 1}
	c, _ := newTestClient(t, backend.handler(t))

	sess, err := c.Login(context.Background(), Credentials{Email: "a@b.test", Password: "x"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got := sess.Tokens().RefreshToken; got != "refresh-0" {
		t.Fatalf("initial refresh token = %q", got)
	}

	var mu sync.Mutex
	var rotations []string
	sess.OnRotate(func(tk Tokens) {
		mu.Lock()
		rotations = append(rotations, tk.RefreshToken)
		mu.Unlock()
	})

	if _, err := c.WithSession(sess).Account(context.Background()); err != nil {
		t.Fatalf("Account: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(rotations) != 1 || rotations[0] != "refresh-1" {
		t.Fatalf("rotations = %v, want [refresh-1]", rotations)
	}
	if got := sess.Tokens().RefreshToken; got != "refresh-1" {
		t.Fatalf("session holds %q after rotation", got)
	}
}

// A rejected refresh token is terminal. Keeping it would make every later call
// present a credential the server has revoked — and if the revocation was a
// reuse detection, presenting it again is evidence of an attack.
func TestRejectedRefreshExpiresTheSessionAndWipesTheToken(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeAPIError(t, w, http.StatusUnauthorized, ErrorCodeUnauthenticated, "revoked")
	}))

	sess := c.Resume("stolen-and-revoked")
	_, err := c.WithSession(sess).Account(context.Background())
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
	// The underlying APIError survives the wrap, so a caller can still quote
	// the request id.
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want a wrapped *APIError", err)
	}
	if got := sess.Tokens().RefreshToken; got != "" {
		t.Fatalf("session still holds a refresh token: %q", got)
	}
}

// A refresh token past the family's absolute end is dead for a reason no retry
// can fix, so it is not presented at all.
func TestExpiredRefreshWindowIsNotPresented(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	sess := c.Resume("rt")
	sess.tokens.RefreshExpiresAt = time.Now().Add(-time.Minute)

	if _, err := c.WithSession(sess).Account(context.Background()); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("%d requests sent with a dead refresh token", got)
	}
}

func TestLogoutClearsLocalStateEvenWhenTheServerFails(t *testing.T) {
	t.Parallel()

	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/login" {
			writeJSON(t, w, http.StatusOK, SessionResponse{
				AccessToken: "a", RefreshToken: "r", ExpiresIn: 900,
				RefreshExpiresAt: time.Now().Add(time.Hour), TokenType: SessionResponseTokenTypeBearer,
			})
			return
		}
		writeAPIError(t, w, http.StatusInternalServerError, ErrorCodeInternal, "boom")
	}), func(o *Options) { o.Retry = RetryPolicy{MaxAttempts: 1} })

	sess, err := c.Login(context.Background(), Credentials{Email: "a@b.test", Password: "x"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := sess.Logout(context.Background()); err == nil {
		t.Fatal("want the server error to be reported")
	}
	// A "logged out" client that still carries a working token is the worst
	// outcome, so the local state is cleared whatever the server said.
	if got := sess.Tokens(); got.RefreshToken != "" || got.AccessToken != "" {
		t.Fatalf("session still holds %v", got)
	}
}

func TestStaticTokenCannotRefresh(t *testing.T) {
	t.Parallel()

	src := StaticToken("abc")
	token, generation, err := src.AccessToken(context.Background())
	if err != nil || token != "abc" || generation != 0 {
		t.Fatalf("AccessToken = %q, %d, %v", token, generation, err)
	}
	if err := src.Refresh(context.Background(), 0); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("Refresh = %v, want ErrSessionExpired", err)
	}
}

// -----------------------------------------------------------------------------
// redaction
// -----------------------------------------------------------------------------

// Redaction has to be structural. A caller who logs a Credentials or a Tokens
// value — with %v, with %s, or through slog — must not be able to leak a
// secret, because "remember not to log this" is not a control.
func TestSecretsRedactInEveryRenderingPath(t *testing.T) {
	t.Parallel()

	creds := Credentials{Email: "sharp@example.test", Password: "correct horse battery staple", TOTPCode: "123456"}
	tokens := Tokens{AccessToken: "jwt.header.payload", RefreshToken: "rt_supersecret"}

	// The values are passed through `any` so that %v and %s go through the
	// same dynamic-dispatch path a caller's log line would, rather than being
	// constant-folded by the vet/staticcheck fast path.
	credsAny, tokensAny := any(creds), any(tokens)
	renderings := []string{
		fmt.Sprintf("%v", credsAny),
		fmt.Sprintf("%s", credsAny),
		fmt.Sprint(credsAny),
		fmt.Sprintf("%v", creds.LogValue()),
		fmt.Sprintf("%v", tokensAny),
		fmt.Sprintf("%s", tokensAny),
		fmt.Sprint(tokensAny),
		fmt.Sprintf("%v", tokens.LogValue()),
	}
	for _, secret := range []string{"correct horse battery staple", "123456", "jwt.header.payload", "rt_supersecret"} {
		for _, rendered := range renderings {
			if strings.Contains(rendered, secret) {
				t.Errorf("secret %q leaked into %q", secret, rendered)
			}
		}
	}
	// The email is not a secret and is the join key an operator needs.
	if !strings.Contains(fmt.Sprint(credsAny), "sharp@example.test") {
		t.Error("the email should survive redaction; it is not a credential")
	}
}

// A transport failure must not carry the URL into the caller's logs.
func TestTransportErrorsAreSanitized(t *testing.T) {
	t.Parallel()

	inner := errors.New("connection refused")
	wrapped := &url.Error{Op: "Get", URL: "https://sharpline.example/api/v1/sports?token=leaked", Err: inner}
	got := sanitizeTransportError(wrapped)
	if got != inner {
		t.Fatalf("sanitizeTransportError = %v, want the inner error", got)
	}
	if strings.Contains(got.Error(), "token=leaked") {
		t.Fatalf("URL survived: %q", got.Error())
	}
	// A non-url.Error passes through untouched.
	if got := sanitizeTransportError(inner); got != inner {
		t.Fatalf("plain error was altered: %v", got)
	}
}

// -----------------------------------------------------------------------------
// money
// -----------------------------------------------------------------------------

func TestFormatMinorIsExactAndUsesNoFloat(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{
		0:                 "0.00",
		1:                 "0.01",
		9:                 "0.09",
		10:                "0.10",
		99:                "0.99",
		100:               "1.00",
		250000:            "2500.00",
		-1:                "-0.01",
		-250050:           "-2500.50",
		9007199254740991:  "90071992547409.91", // the schema's bound, 2^53-1
		-9007199254740991: "-90071992547409.91",
		math.MaxInt64:     "92233720368547758.07",
		math.MinInt64:     "-92233720368547758.08",
	}
	for in, want := range cases {
		if got := FormatMinor(in); got != want {
			t.Errorf("FormatMinor(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestCallSafetyIsOnlyGetAndHead(t *testing.T) {
	t.Parallel()

	for method, want := range map[string]bool{
		http.MethodGet:    true,
		http.MethodHead:   true,
		http.MethodPost:   false,
		http.MethodPut:    false,
		http.MethodPatch:  false,
		http.MethodDelete: false,
	} {
		if got := (call{method: method}).safe(); got != want {
			t.Errorf("%s safe = %v, want %v", method, got, want)
		}
	}
}

// -----------------------------------------------------------------------------
// the endpoint map
// -----------------------------------------------------------------------------

// Every method must hit the path and verb openapi.yaml declares for it, and
// must send a credential exactly when the spec marks the operation
// `security: [{bearerAuth: []}]`.
//
// This is the SDK's half of the contract the server proves with its own route
// test. Without it, a renamed path is a 404 a caller discovers in production;
// with it, the two halves cannot drift silently.
func TestEveryEndpointUsesItsDocumentedMethodAndPath(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	amount := MoneyMinor(50_000)

	cases := []struct {
		name       string
		wantMethod string
		wantPath   string
		wantAuth   bool
		invoke     func(context.Context, *Client) error
	}{
		{"listSports", http.MethodGet, "/api/v1/sports", false,
			func(ctx context.Context, c *Client) error { _, err := c.Sports(ctx); return err }},
		{"listLeaguesInSport", http.MethodGet, "/api/v1/sports/nfl/leagues", false,
			func(ctx context.Context, c *Client) error { _, err := c.Leagues(ctx, "nfl"); return err }},
		{"listBooks", http.MethodGet, "/api/v1/books", false,
			func(ctx context.Context, c *Client) error { _, err := c.Books(ctx); return err }},
		{"getBoard", http.MethodGet, "/api/v1/board", false,
			func(ctx context.Context, c *Client) error { _, err := c.Board(ctx, GetBoardParams{}); return err }},
		{"getLeagueBoard", http.MethodGet, "/api/v1/leagues/nfl/board", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.LeagueBoard(ctx, "nfl", GetLeagueBoardParams{})
				return err
			}},
		{"getEvent", http.MethodGet, "/api/v1/events/evt_1", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.Event(ctx, "evt_1", GetEventParams{})
				return err
			}},
		{"compareMarketPrices", http.MethodGet, "/api/v1/markets/mkt_1/prices", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.CompareMarketPrices(ctx, "mkt_1", CompareMarketPricesParams{})
				return err
			}},
		{"getSelectionHistory", http.MethodGet, "/api/v1/selections/sel_1/history", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.SelectionHistory(ctx, "sel_1", GetSelectionHistoryParams{Book: "pinnacle", From: from})
				return err
			}},
		{"searchEvents", http.MethodGet, "/api/v1/search", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.Search(ctx, SearchEventsParams{Q: "chiefs"})
				return err
			}},

		{"register", http.MethodPost, "/api/v1/auth/register", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.Register(ctx, Credentials{Email: "a@b.test", Password: "correct horse battery"})
				return err
			}},
		{"login", http.MethodPost, "/api/v1/auth/login", false,
			func(ctx context.Context, c *Client) error {
				_, err := c.Login(ctx, Credentials{Email: "a@b.test", Password: "x"})
				return err
			}},
		{"refreshSession", http.MethodPost, "/api/v1/auth/refresh", false,
			func(ctx context.Context, c *Client) error { _, err := c.refreshSession(ctx, "rt"); return err }},
		{"logout", http.MethodPost, "/api/v1/auth/logout", false,
			func(ctx context.Context, c *Client) error { return c.logout(ctx, "rt") }},

		{"getAccount", http.MethodGet, "/api/v1/account", true,
			func(ctx context.Context, c *Client) error { _, err := c.Account(ctx); return err }},
		{"getBalance", http.MethodGet, "/api/v1/account/balance", true,
			func(ctx context.Context, c *Client) error { _, err := c.Balance(ctx); return err }},
		{"listLimits", http.MethodGet, "/api/v1/account/limits", true,
			func(ctx context.Context, c *Client) error { _, err := c.Limits(ctx); return err }},
		{"setLimit", http.MethodPost, "/api/v1/account/limits", true,
			func(ctx context.Context, c *Client) error {
				_, err := c.SetLimit(ctx, SetLimitRequest{
					Kind: LimitKindLoss, Period: LimitPeriodWeek, AmountMinor: &amount,
				})
				return err
			}},
		{"beginTOTPEnrolment", http.MethodPost, "/api/v1/account/totp", true,
			func(ctx context.Context, c *Client) error { _, err := c.BeginTOTPEnrolment(ctx); return err }},
		{"removeTOTP", http.MethodDelete, "/api/v1/account/totp", true,
			func(ctx context.Context, c *Client) error { return c.RemoveTOTP(ctx, "123456") }},
		{"confirmTOTPEnrolment", http.MethodPost, "/api/v1/account/totp/confirm", true,
			func(ctx context.Context, c *Client) error {
				_, err := c.ConfirmTOTPEnrolment(ctx, "123456")
				return err
			}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotMethod, gotPath, gotAuth string
			c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				// `{}` decodes into every response type in the spec, which is
				// what lets one handler serve the whole table.
				_, _ = w.Write([]byte(`{}`))
			}), func(o *Options) { o.Tokens = StaticToken("access-token") })

			if err := tc.invoke(context.Background(), c); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if gotMethod != tc.wantMethod {
				t.Errorf("method = %s, want %s", gotMethod, tc.wantMethod)
			}
			if gotPath != tc.wantPath {
				t.Errorf("path = %s, want %s", gotPath, tc.wantPath)
			}
			// A public operation that sent a credential would leak a token to
			// an endpoint that never needed one; an authenticated one that did
			// not would be a 401 the caller cannot diagnose.
			if hasAuth := gotAuth != ""; hasAuth != tc.wantAuth {
				t.Errorf("Authorization present = %v, want %v", hasAuth, tc.wantAuth)
			}
		})
	}
}

// The generated enums are the spec's closed value sets. Asserting that every
// declared constant is a member — and that an invented value is not — is what
// makes ErrorCode.Valid() usable as a guard rather than decoration.
func TestGeneratedEnumsAcceptOnlyTheirDeclaredMembers(t *testing.T) {
	t.Parallel()

	type validator interface{ Valid() bool }

	// The value sets CLAUDE.md pins by name are enumerated in full: §6 fixes the
	// account statuses and the responsible-gaming limit kinds and periods, §4
	// fixes the odds formats, and the error codes are the branch a client
	// writes. The catalogue's own vocabularies (market type, book kind, event
	// and market status, selection role) are the provider-facing half of the
	// domain and are still being widened, so they are checked below for
	// REJECTING what they must never accept rather than for a membership list
	// that would have to be edited every time the catalogue grows.
	members := []validator{
		ErrorCodeBadRequest, ErrorCodeInvalidParameter, ErrorCodeInvalidCursor,
		ErrorCodeUnauthenticated, ErrorCodeInvalidCredentials, ErrorCodeTotpRequired,
		ErrorCodeInvalidTotpCode, ErrorCodeForbidden, ErrorCodeAccountNotActive,
		ErrorCodeNotFound, ErrorCodeConflict, ErrorCodeAlreadyExists,
		ErrorCodeUnprocessable, ErrorCodeRateLimited, ErrorCodeInternal,

		AccountStatusActive, AccountStatusSuspended, AccountStatusSelfExcluded, AccountStatusClosed,
		LimitKindGrant, LimitKindStake, LimitKindLoss, LimitKindSession,
		LimitPeriodDay, LimitPeriodWeek, LimitPeriodMonth, LimitPeriodSession,
		OddsFormatDecimal, OddsFormatAmerican, OddsFormatFractional,
		SessionResponseTokenTypeBearer, BalanceResponseCurrencyPLAY,
	}
	for i, m := range members {
		if !m.Valid() {
			t.Errorf("members[%d] = %v is rejected by its own Valid()", i, m)
		}
	}

	invented := []validator{
		ErrorCode("method_not_allowed"), AccountStatus("verified"), LimitKind("deposit"),
		LimitPeriod("year"), OddsFormat("hong_kong"), EventStatus("abandoned"),
		MarketStatus("void"), MarketType("teaser"), SelectionRole("tie"),
		BookKind("bookmaker"), SessionResponseTokenType("Basic"), BalanceResponseCurrency("USD"),
	}
	for i, v := range invented {
		if v.Valid() {
			t.Errorf("invented[%d] = %v was accepted", i, v)
		}
	}

	// LimitKind has no `deposit` and never will: this system has no deposits
	// (CLAUDE.md §0), and a constant for a control that can never fire would
	// invite someone to build the flow it implies.
	if LimitKind("deposit").Valid() {
		t.Error("LimitKind accepts 'deposit'")
	}
	// BalanceResponse currency is PLAY, not an ISO 4217 code: labelling play
	// money USD is the first step toward a client treating it as money.
	if BalanceResponseCurrency("USD").Valid() {
		t.Error("BalanceResponseCurrency accepts a real currency code")
	}
}
