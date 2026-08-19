package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func mustTrusted(t *testing.T, cidrs ...string) TrustedProxies {
	t.Helper()
	tp, err := ParseTrustedProxies(cidrs)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", cidrs, err)
	}
	return tp
}

// TestClientAddr is the forged-header test. Everything in here is a rule that,
// got wrong, makes per-IP rate limiting an honour system.
func TestClientAddr(t *testing.T) {
	t.Parallel()

	// The compose bridge network the `proxy` container sits on.
	proxyNet := mustTrusted(t, "172.18.0.0/16")

	cases := []struct {
		name       string
		trusted    TrustedProxies
		remoteAddr string
		headers    map[string]string
		want       string
	}{
		{
			name:       "no trusted proxies: headers are ignored entirely",
			trusted:    nil,
			remoteAddr: "203.0.113.9:44100",
			headers:    map[string]string{headerRealIP: "1.1.1.1", headerForwardedFor: "1.1.1.1"},
			want:       "203.0.113.9",
		},
		{
			name:       "untrusted peer cannot forge an address",
			trusted:    proxyNet,
			remoteAddr: "198.51.100.4:44100",
			headers:    map[string]string{headerRealIP: "10.0.0.1", headerForwardedFor: "10.0.0.1"},
			want:       "198.51.100.4",
		},
		{
			name:       "trusted peer: X-Real-IP is believed",
			trusted:    proxyNet,
			remoteAddr: "172.18.0.5:44100",
			headers:    map[string]string{headerRealIP: "203.0.113.77"},
			want:       "203.0.113.77",
		},
		{
			name:       "trusted peer: X-Forwarded-For is read RIGHT to left",
			trusted:    proxyNet,
			remoteAddr: "172.18.0.5:44100",
			// The client wrote the leftmost entry itself. Reading left-to-right
			// — the common mistake — would return the attacker's value.
			headers: map[string]string{headerForwardedFor: "9.9.9.9, 203.0.113.88"},
			want:    "203.0.113.88",
		},
		{
			name:       "trusted peer: trusted hops in XFF are skipped",
			trusted:    proxyNet,
			remoteAddr: "172.18.0.5:44100",
			headers:    map[string]string{headerForwardedFor: "203.0.113.88, 172.18.0.9, 172.18.0.5"},
			want:       "203.0.113.88",
		},
		{
			name:       "trusted peer with no forwarding headers falls back to the peer",
			trusted:    proxyNet,
			remoteAddr: "172.18.0.5:44100",
			want:       "172.18.0.5",
		},
		{
			name:       "trusted peer: an unparseable X-Real-IP falls through to XFF",
			trusted:    proxyNet,
			remoteAddr: "172.18.0.5:44100",
			headers:    map[string]string{headerRealIP: "not-an-address", headerForwardedFor: "203.0.113.5"},
			want:       "203.0.113.5",
		},
		{
			name:       "IPv4-mapped IPv6 is normalised",
			trusted:    nil,
			remoteAddr: "[::ffff:203.0.113.9]:44100",
			want:       "203.0.113.9",
		},
		{
			name:       "IPv6 zone is stripped",
			trusted:    nil,
			remoteAddr: "[fe80::1%eth0]:44100",
			want:       "fe80::1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			got := clientAddr(r, tc.trusted)
			if got.String() != tc.want {
				t.Fatalf("clientAddr = %q, want %q", got.String(), tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	tp, err := ParseTrustedProxies([]string{"172.18.0.0/16", " 10.1.2.3 ", "", "2001:db8::/32"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(tp) != 3 {
		t.Fatalf("got %d prefixes, want 3 (the empty entry must be skipped)", len(tp))
	}

	// A bare address becomes a single-host prefix, so a neighbouring address is
	// NOT trusted.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.4:1000"
	r.Header.Set(headerRealIP, "1.2.3.4")
	if got := clientAddr(r, tp); got.String() != "10.1.2.4" {
		t.Fatalf("10.1.2.4 was treated as trusted; a bare address must be a /32")
	}

	if _, err := ParseTrustedProxies([]string{"not-a-cidr"}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("ParseTrustedProxies(bad) = %v, want ErrInvalidOptions", err)
	}
}

func TestRateLimitSubject(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.9:1"
	if got := rateLimitSubject(clientAddr(r, nil)); got != "203.0.113.9" {
		t.Fatalf("IPv4 subject = %q, want the bare address", got)
	}

	// An address that could not be determined must NOT be exempt.
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = ""
	if got := rateLimitSubject(clientAddr(r2, nil)); got != "unknown" {
		t.Fatalf("subject for an undeterminable address = %q, want \"unknown\" — never a bypass", got)
	}
}

func TestClientAddrMiddlewareStoresOneAnswer(t *testing.T) {
	t.Parallel()

	var seen string
	h := Chain(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr, ok := ClientIPFrom(r.Context())
		if !ok {
			t.Error("no client address in the context")
			return
		}
		seen = addr.String()
	}), ClientAddr(mustTrusted(t, "172.18.0.0/16")))

	r := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil)
	r.RemoteAddr = "172.18.0.5:1"
	r.Header.Set(headerRealIP, "203.0.113.42")
	h.ServeHTTP(httptest.NewRecorder(), r)

	if seen != "203.0.113.42" {
		t.Fatalf("handler saw %q, want 203.0.113.42", seen)
	}
}
