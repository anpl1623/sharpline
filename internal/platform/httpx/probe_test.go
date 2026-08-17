package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIsProbeInvocation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "no args", args: []string{"/usr/local/bin/sharpline"}, want: false},
		{name: "probe subcommand", args: []string{"/usr/local/bin/sharpline", "healthcheck"}, want: true},
		{name: "other subcommand", args: []string{"/usr/local/bin/sharpline", "serve"}, want: false},
		{name: "probe not first arg", args: []string{"/usr/local/bin/sharpline", "serve", "healthcheck"}, want: false},
		{name: "empty argv", args: nil, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsProbeInvocation(tc.args); got != tc.want {
				t.Fatalf("IsProbeInvocation(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestLoopbackAddr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		addr    string
		want    string
		wantErr bool
	}{
		// The forms compose actually sets: SHARPLINE_HTTP_ADDR=":8080".
		{name: "port only", addr: ":8080", want: "127.0.0.1:8080"},
		{name: "ipv4 wildcard", addr: "0.0.0.0:8081", want: "127.0.0.1:8081"},
		{name: "ipv6 wildcard", addr: "[::]:8082", want: "127.0.0.1:8082"},
		{name: "surrounding space", addr: "  :8083  ", want: "127.0.0.1:8083"},
		// A concrete host is left alone: a service deliberately bound to one
		// interface must be probed on that interface, not on loopback.
		{name: "explicit host preserved", addr: "10.0.0.5:8084", want: "10.0.0.5:8084"},
		{name: "explicit loopback preserved", addr: "127.0.0.1:8085", want: "127.0.0.1:8085"},

		{name: "empty", addr: "", wantErr: true},
		{name: "no port", addr: "127.0.0.1", wantErr: true},
		{name: "no colon", addr: "8080", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := LoopbackAddr(tc.addr)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("LoopbackAddr(%q) = %q, want error", tc.addr, got)
				}
				if !errors.Is(err, ErrInvalidOptions) {
					t.Fatalf("LoopbackAddr(%q) error = %v, want ErrInvalidOptions", tc.addr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoopbackAddr(%q) unexpected error: %v", tc.addr, err)
			}
			if got != tc.want {
				t.Fatalf("LoopbackAddr(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestRunProbeAgainstRealServer exercises the probe end to end against a real
// listener, which is the only way to prove the thing the Docker healthcheck
// depends on: a 200 exits 0 and a 503 exits 1.
func TestRunProbeAgainstRealServer(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "ready", status: http.StatusOK, wantErr: false},
		{name: "not ready", status: http.StatusServiceUnavailable, wantErr: true},
		{name: "server error", status: http.StatusInternalServerError, wantErr: true},
		{name: "not found", status: http.StatusNotFound, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != PathReadyz {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
			if err != nil {
				t.Fatalf("splitting test server address %q: %v", srv.URL, err)
			}

			err = RunProbe(context.Background(), ":"+port, PathReadyz, time.Second)
			if tc.wantErr && err == nil {
				t.Fatalf("RunProbe against %d = nil, want error", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("RunProbe against %d = %v, want nil", tc.status, err)
			}
		})
	}
}

// TestRunProbeNoListener is the failure mode that matters most: the container
// is up but the process is not accepting connections. The probe must report
// that rather than hang until Docker kills it.
func TestRunProbeNoListener(t *testing.T) {
	t.Parallel()

	// Bind and immediately release, so the port is real but certainly closed.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("splitting %q: %v", addr, err)
	}

	if err := RunProbe(context.Background(), ":"+port, PathReadyz, time.Second); err == nil {
		t.Fatal("RunProbe against a closed port = nil, want error")
	}
}

// TestProbeExitCodes covers the contract Docker consumes: exit 0 healthy,
// exit 1 unhealthy, with the env var taking precedence over the fallback.
func TestProbeExitCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathReadyz {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("splitting test server address %q: %v", srv.URL, err)
	}

	const envName = "SHARPLINE_TEST_HTTP_ADDR"

	t.Run("env addr wins and reports healthy", func(t *testing.T) {
		t.Setenv(envName, ":"+port)
		// Fallback deliberately points somewhere dead: if it were used, the
		// probe would fail and this assertion would catch it.
		if code := Probe(context.Background(), envName, ":1", PathReadyz, os.Stderr); code != 0 {
			t.Fatalf("Probe exit code = %d, want 0", code)
		}
	})

	t.Run("falls back when env unset", func(t *testing.T) {
		t.Setenv(envName, "")
		if code := Probe(context.Background(), envName, ":"+port, PathReadyz, os.Stderr); code != 0 {
			t.Fatalf("Probe exit code = %d, want 0", code)
		}
	})

	t.Run("unreachable reports unhealthy", func(t *testing.T) {
		t.Setenv(envName, ":1")
		if code := Probe(context.Background(), envName, ":1", PathReadyz, os.Stderr); code != 1 {
			t.Fatalf("Probe exit code = %d, want 1", code)
		}
	})
}

// TestPublicPrefixMirrorsProbesOnly is the security-relevant half of the
// PublicPrefix option: /healthz and /readyz appear beneath the prefix, and
// /metrics does NOT — the proxy's `handle /metrics*` deny rule only covers the
// site root, so a mirrored /api/metrics would be publicly scrapeable.
func TestPublicPrefixMirrorsProbesOnly(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerOptions{
		Service:      "api",
		Addr:         ":8080",
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		PublicPrefix: "/api",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	tests := []struct {
		path string
		want int
	}{
		{path: PathHealthz, want: http.StatusOK},
		{path: PathReadyz, want: http.StatusOK},
		{path: PathMetrics, want: http.StatusOK},
		{path: "/api" + PathHealthz, want: http.StatusOK},
		{path: "/api" + PathReadyz, want: http.StatusOK},
		// The whole point.
		{path: "/api" + PathMetrics, want: http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if rec.Code != tc.want {
				t.Fatalf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
			}
		})
	}
}

// TestPublicPrefixAbsentByDefault: a service that sets no prefix must not
// acquire one. Only `api` sits behind a prefix-preserving proxy route.
func TestPublicPrefixAbsentByDefault(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerOptions{
		Service: "pricer",
		Addr:    ":8082",
		Logger:  slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api"+PathHealthz, nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/healthz on a prefix-less service = %d, want 404", rec.Code)
	}
}

// TestPublicPrefixTrailingSlash: "/api/" and "/api" must produce the same
// routes. net/http treats "/api//healthz" as a distinct, unmatched path.
func TestPublicPrefixTrailingSlash(t *testing.T) {
	t.Parallel()

	srv, err := NewServer(ServerOptions{
		Service:      "api",
		Addr:         ":8080",
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
		PublicPrefix: "/api/",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api"+PathHealthz, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/healthz with PublicPrefix=\"/api/\" = %d, want 200", rec.Code)
	}
}
