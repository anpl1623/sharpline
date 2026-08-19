package middleware

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anpl1623/sharpline/internal/domain"
)

// Values that must never appear in a log line. They are shaped like the real
// thing so a substring search is meaningful.
const (
	secretToken   = "eyJhbGciOiJIUzI1NiJ9.SECRET-ACCESS-TOKEN-PAYLOAD.c2lnbmF0dXJl"
	secretCookie  = "SESSION-COOKIE-VALUE-abcdef0123456789"
	secretQuery   = "QUERY-STRING-BEARER-TOKEN"
	secretBody    = "hunter2-the-password"
	secretRefresh = "REFRESH-TOKEN-VALUE"
)

// TestAccessLogNeverLogsACredential is the structural control described in the
// package doc, asserted rather than promised.
//
// The request below carries a credential in every place a client can put one:
// the Authorization header, a session cookie, a second cookie, the query string,
// a Proxy-Authorization header and the body. The emitted log line must contain
// none of them.
//
// If somebody later adds slog.Any("headers", r.Header) — the one-line change
// that leaks every token in production — this test fails.
func TestAccessLogNeverLogsACredential(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	h := Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
		AccessLog(log),
	)

	r := httptest.NewRequest(http.MethodPost,
		"/api/v1/auth/login?access_token="+secretQuery, strings.NewReader(`{"password":"`+secretBody+`"}`))
	r.Header.Set("Authorization", "Bearer "+secretToken)
	r.Header.Set("Proxy-Authorization", "Basic "+secretToken)
	r.Header.Set("Cookie", "sharpline_session="+secretCookie+"; refresh="+secretRefresh)
	r.Header.Set("User-Agent", "Mozilla/5.0")

	h.ServeHTTP(httptest.NewRecorder(), r)

	line := buf.String()
	if line == "" {
		t.Fatal("no log line was emitted")
	}

	for _, secret := range []string{secretToken, secretCookie, secretQuery, secretBody, secretRefresh} {
		if strings.Contains(line, secret) {
			t.Fatalf("the access log leaked a credential.\nsecret: %s\nline:   %s", secret, line)
		}
	}
	// The two header NAMES are equally telling: their presence means headers
	// are being enumerated somewhere.
	for _, header := range []string{"Authorization", "authorization", "Cookie", "cookie", "Proxy-Authorization"} {
		if strings.Contains(line, header) {
			t.Fatalf("the access log emitted a header field; headers must never be enumerated.\nfound: %s\nline:  %s", header, line)
		}
	}

	// It must still be useful.
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	for _, field := range []string{"method", "path", "status", "duration_ms"} {
		if _, ok := rec[field]; !ok {
			t.Fatalf("log line is missing %q: %s", field, line)
		}
	}
	if got := rec["path"]; got != "/api/v1/auth/login" {
		t.Fatalf("path = %v, want the path WITHOUT the query string", got)
	}
}

func TestAccessLogLevelTracksStatus(t *testing.T) {
	t.Parallel()

	cases := []struct {
		status int
		want   string
	}{
		{http.StatusOK, "INFO"},
		{http.StatusUnauthorized, "WARN"},
		{http.StatusTooManyRequests, "WARN"},
		{http.StatusInternalServerError, "ERROR"},
	}

	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			h := Chain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}), AccessLog(log))

			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))

			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatalf("log line is not JSON: %v", err)
			}
			if rec["level"] != tc.want {
				t.Fatalf("level = %v for status %d, want %s (raising SHARPLINE_LOG_LEVEL must not hide failures)",
					rec["level"], tc.status, tc.want)
			}
		})
	}
}

// TestAccessLogRecordsIdentityEstablishedDownstream: the identity is read after
// the handler returns, so a request authenticated deeper in the chain still logs
// its user id.
func TestAccessLogRecordsIdentityEstablishedDownstream(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	establish := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := withIdentity(r.Context(), Identity{UserID: domain.UserID("usr_abc")})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}

	h := Chain(inner, AccessLog(log), establish)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/account", nil))

	if !strings.Contains(buf.String(), `"user_id":"usr_abc"`) {
		t.Fatalf("access log did not record the identity: %s", buf.String())
	}
}

func TestTruncateMarksWhatItCut(t *testing.T) {
	t.Parallel()
	got := truncate(strings.Repeat("x", maxUserAgentLen+50), maxUserAgentLen)
	if len(got) != maxUserAgentLen+3 || !strings.HasSuffix(got, "...") {
		t.Fatalf("truncate produced %d bytes ending %q; a truncated value must be marked as truncated",
			len(got), got[len(got)-3:])
	}
	if s := "short"; truncate(s, maxUserAgentLen) != s {
		t.Fatal("truncate modified a value inside the limit")
	}
}
