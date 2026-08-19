package middleware

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anpl1623/sharpline/internal/domain"
	"github.com/prometheus/client_golang/prometheus"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// okAuthenticator accepts one token and rejects everything else.
func okAuthenticator(valid string, id Identity) AuthenticatorFunc {
	return func(_ context.Context, token string) (Identity, error) {
		if token != valid {
			return Identity{}, fmt.Errorf("%w: no such token", ErrInvalidCredential)
		}
		return id, nil
	}
}

func testIdentity() Identity {
	return Identity{
		UserID:    domain.UserID("usr_01HZ"),
		SessionID: "fam_01HZ",
		IssuedAt:  time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(10 * time.Minute),
		AMR:       []string{"pwd", "totp"},
	}
}

func authChain(t *testing.T, a Authenticator, inner http.Handler) (http.Handler, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}
	mw, err := Authenticate(AuthOptions{Authenticator: a, Metrics: m})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	return Chain(inner, RequestID(), mw), reg
}

// TestAuthenticateFailsClosedOnVerifierError is the single most important test
// in this package.
//
// A verifier that fails for an INTERNAL reason — a database timeout, a missing
// signing key, a panic in the JWT library — must produce 401, not a pass-through
// to an anonymous handler. `if err != nil { next.ServeHTTP(...) }` is a
// one-character-looking mistake that turns every verifier outage into an
// authentication bypass.
func TestAuthenticateFailsClosedOnVerifierError(t *testing.T) {
	t.Parallel()

	internalFailure := errors.New("connection refused: token store unreachable")

	reached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	h, reg := authChain(t, AuthenticatorFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, internalFailure
	}), inner)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.Header.Set("Authorization", "Bearer "+secretToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if reached {
		t.Fatal("the handler ran despite an error from the verifier — this is an authentication bypass")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatalf("the internal error reached the client: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretToken) {
		t.Fatalf("the response echoed the token: %s", w.Body.String())
	}
	if got := counterValue(t, reg, "sharpline_http_auth_total", map[string]string{"outcome": authRejected}); got != 1 {
		t.Fatalf("auth_total{outcome=rejected} = %v, want 1", got)
	}
}

// TestAuthenticateFailsClosedOnEmptyIdentity: a verifier returning no error and
// no user is a bug in the verifier, and treating it as anonymous would be the
// same bypass by another route.
func TestAuthenticateFailsClosedOnEmptyIdentity(t *testing.T) {
	t.Parallel()

	reached := false
	h, _ := authChain(t, AuthenticatorFunc(func(context.Context, string) (Identity, error) {
		return Identity{}, nil
	}), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
	}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.Header.Set("Authorization", "Bearer whatever")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if reached {
		t.Fatal("an empty identity with no error was treated as a valid anonymous request")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// TestAuthResponsesAreIndistinguishable: CLAUDE.md's no-enumeration requirement.
// An unknown account, a wrong password, an expired token and a revoked session
// must be one response, byte for byte, with the same headers.
func TestAuthResponsesAreIndistinguishable(t *testing.T) {
	t.Parallel()

	causes := []error{
		fmt.Errorf("%w: user not found", ErrInvalidCredential),
		fmt.Errorf("%w: signature mismatch", ErrInvalidCredential),
		fmt.Errorf("%w: token expired at 2026-08-19T00:00:00Z", ErrInvalidCredential),
		fmt.Errorf("%w: refresh family revoked (reuse_detected)", ErrInvalidCredential),
		errors.New("postgres: dial tcp 10.0.0.5:5432: i/o timeout"),
	}

	var first []byte
	var firstHeaders http.Header

	for i, cause := range causes {
		h, _ := authChain(t, AuthenticatorFunc(func(context.Context, string) (Identity, error) {
			return Identity{}, cause
		}), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

		r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
		r.Header.Set("Authorization", "Bearer token-"+fmt.Sprint(i))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)

		// The request id legitimately varies; strip it before comparing.
		body := bytes.ReplaceAll(w.Body.Bytes(), []byte(w.Header().Get(headerRequestID)), []byte("RID"))

		headers := w.Header().Clone()
		headers.Del(headerRequestID)

		if i == 0 {
			first, firstHeaders = body, headers
			continue
		}
		if !bytes.Equal(first, body) {
			t.Fatalf("cause %d produced a distinguishable body.\nfirst: %s\nthis:  %s\ncause: %v",
				i, first, body, cause)
		}
		if fmt.Sprint(firstHeaders) != fmt.Sprint(headers) {
			t.Fatalf("cause %d produced distinguishable headers.\nfirst: %v\nthis:  %v", i, firstHeaders, headers)
		}
	}
}

func TestAuthenticateAcceptsAValidToken(t *testing.T) {
	t.Parallel()

	want := testIdentity()
	var got Identity

	h, reg := authChain(t, okAuthenticator(secretToken, want),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id, ok := IdentityFrom(r.Context())
			if !ok {
				t.Error("no identity in the handler's context")
			}
			got = id
			w.WriteHeader(http.StatusOK)
		}))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.Header.Set("Authorization", "Bearer "+secretToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got.UserID != want.UserID || got.SessionID != want.SessionID {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
	if !got.HasAMR("totp") {
		t.Fatal("AMR did not survive into the context")
	}
	if v := counterValue(t, reg, "sharpline_http_auth_total", map[string]string{"outcome": authOK}); v != 1 {
		t.Fatalf("auth_total{outcome=ok} = %v, want 1", v)
	}
}

// TestAuthenticateAbsentCredentialIsAnonymous: the catalogue read surface
// (CLAUDE.md §6, Core) is public, so no credential must not be an error.
func TestAuthenticateAbsentCredentialIsAnonymous(t *testing.T) {
	t.Parallel()

	reached := false
	h, reg := authChain(t, okAuthenticator(secretToken, testIdentity()),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			if _, ok := IdentityFrom(r.Context()); ok {
				t.Error("an anonymous request carried an identity")
			}
			w.WriteHeader(http.StatusOK)
		}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

	if !reached {
		t.Fatal("a public endpoint rejected an anonymous request")
	}
	if v := counterValue(t, reg, "sharpline_http_auth_total", map[string]string{"outcome": authAbsent}); v != 1 {
		t.Fatalf("auth_total{outcome=absent} = %v, want 1", v)
	}
}

// TestMalformedCredentialIs401: a client that believes it is authenticated must
// not be silently downgraded to anonymous — that turns "my token was garbage"
// into "the API returned somebody else's empty account".
func TestMalformedCredentialIs401(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		header string
	}{
		{"basic scheme", "Basic dXNlcjpwYXNz"},
		{"no scheme", secretToken},
		{"bearer with no token", "Bearer "},
		{"bearer with a space in the token", "Bearer abc def"},
		{"credential list", "Bearer abc, Basic def"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reached := false
			h, _ := authChain(t, okAuthenticator(secretToken, testIdentity()),
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))

			r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
			r.Header.Set("Authorization", tc.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if reached {
				t.Fatal("a malformed credential was treated as anonymous")
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}

	// Case-insensitivity is required by RFC 7235 and real clients send "bearer".
	t.Run("lowercase scheme is accepted", func(t *testing.T) {
		t.Parallel()
		reached := false
		h, _ := authChain(t, okAuthenticator(secretToken, testIdentity()),
			http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { reached = true; w.WriteHeader(200) }))

		r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
		r.Header.Set("Authorization", "bearer "+secretToken)
		h.ServeHTTP(httptest.NewRecorder(), r)

		if !reached {
			t.Fatal("a lowercase bearer scheme was rejected")
		}
	})
}

func TestAuthCookieIsOffByDefault(t *testing.T) {
	t.Parallel()

	// Without CookieName, a cookie-borne token must be invisible: the request
	// is anonymous, not authenticated.
	mw, err := Authenticate(AuthOptions{Authenticator: okAuthenticator(secretToken, testIdentity())})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	var authenticated bool
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authenticated = IdentityFrom(r.Context())
	}), mw)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.AddCookie(&http.Cookie{Name: "sharpline_session", Value: secretToken})
	h.ServeHTTP(httptest.NewRecorder(), r)

	if authenticated {
		t.Fatal("a cookie was accepted as a credential with CookieName unset; that is CSRF surface nobody opted into")
	}

	// With it set, the same cookie authenticates.
	mw, err = Authenticate(AuthOptions{
		Authenticator: okAuthenticator(secretToken, testIdentity()),
		CookieName:    "sharpline_session",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	h = Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, authenticated = IdentityFrom(r.Context())
	}), mw)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if !authenticated {
		t.Fatal("CookieName was set and the cookie was still ignored")
	}
}

func TestRequireIdentity(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	m, err := NewMetrics(reg)
	if err != nil {
		t.Fatalf("NewMetrics: %v", err)
	}

	reached := false
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}), RequireIdentity(m, nil))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/account", nil))

	if reached {
		t.Fatal("an anonymous request reached a handler behind RequireIdentity")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if v := counterValue(t, reg, "sharpline_http_auth_total", map[string]string{"outcome": authRequired}); v != 1 {
		t.Fatalf("auth_total{outcome=required} = %v, want 1", v)
	}
}

func TestAuthenticateRejectsNilAuthenticator(t *testing.T) {
	t.Parallel()
	if _, err := Authenticate(AuthOptions{}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("Authenticate(nil) = %v, want ErrInvalidOptions", err)
	}
}

// TestAuthLogNeverContainsTheToken: the verifier's error goes to the log, and
// the token must not go with it.
func TestAuthLogNeverContainsTheToken(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	mw, err := Authenticate(AuthOptions{
		Authenticator: AuthenticatorFunc(func(context.Context, string) (Identity, error) {
			return Identity{}, fmt.Errorf("%w: expired", ErrInvalidCredential)
		}),
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	h := Chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		RequestID(), Correlate(log), mw)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/account", nil)
	r.Header.Set("Authorization", "Bearer "+secretToken)
	h.ServeHTTP(httptest.NewRecorder(), r)

	if strings.Contains(buf.String(), secretToken) {
		t.Fatalf("the rejection log line contains the token: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "expired") {
		t.Fatalf("the rejection log line lost the cause, which is the whole point of logging it: %s", buf.String())
	}
}

func TestIdentityLogValueCarriesNoSecret(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	log.Info("identity", slog.Any("identity", testIdentity()))

	if !strings.Contains(buf.String(), "usr_01HZ") {
		t.Fatalf("Identity.LogValue dropped the user id: %s", buf.String())
	}
	if strings.Contains(buf.String(), "AMR") || strings.Contains(buf.String(), "totp") {
		t.Fatalf("Identity.LogValue emitted an unexpected field: %s", buf.String())
	}
}
