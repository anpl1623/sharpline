package httpx

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// ProbeSubcommand is the argv[1] value that turns any Sharpline binary into a
// one-shot health prober instead of a long-running server.
//
// This exists to solve a specific, real constraint. Every Go service ships in
// gcr.io/distroless/static:nonroot, which by design contains no shell, no wget
// and no curl — so a Docker `healthcheck:` has nothing to invoke and the
// containers can never leave the "no healthcheck" state. Compose then has to
// gate dependents on `service_started` ("the process was spawned"), which is
// not readiness and lets the proxy come up in front of a listener that is not
// yet accepting connections.
//
// The binary itself is the one executable that IS present in the image, so the
// binary becomes the probe:
//
//	healthcheck:
//	  test: ["CMD", "/usr/local/bin/sharpline", "healthcheck"]
//
// This is the same trick Kubernetes users reach for with `exec` probes on
// distroless images, and it keeps the image free of a second binary.
const ProbeSubcommand = "healthcheck"

// DefaultProbeTimeout bounds the whole self-probe: dial, request and response.
// Deliberately shorter than the `timeout:` any caller should configure on the
// Docker healthcheck, so the process exits on its own terms with a readable
// error rather than being killed mid-request.
const DefaultProbeTimeout = 3 * time.Second

// IsProbeInvocation reports whether this process was started as a health probe
// rather than as a server. Call it as the first statement in main, before
// config.Load: the probe must work even when a dependency-related config
// variable is missing, since diagnosing that is exactly when a probe matters.
func IsProbeInvocation(args []string) bool {
	return len(args) > 1 && args[1] == ProbeSubcommand
}

// RunProbe performs the self-probe and returns nil when the service reports
// itself ready.
//
// addr is the service's own listen address in net/http form (":8080",
// "0.0.0.0:8080"); it is rewritten to dial the loopback interface, because the
// probe runs inside the very container it is probing and must not depend on
// the container's own DNS name or on the wildcard bind being externally
// reachable.
//
// path is normally PathReadyz. Readiness, not liveness, is the right signal for
// a Docker healthcheck: it is what `depends_on: condition: service_healthy`
// consumes, and once dependency Checkers are registered it is the only one of
// the two that can ever go false.
func RunProbe(ctx context.Context, addr, path string, timeout time.Duration) error {
	dialAddr, err := LoopbackAddr(addr)
	if err != nil {
		return err
	}
	if path == "" {
		path = PathReadyz
	}
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	url := "http://" + dialAddr + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("httpx: build probe request for %s: %w", url, err)
	}

	// A dedicated transport with keepalives disabled: this process makes
	// exactly one request and then exits, so a pooled connection is pure
	// overhead and would keep a socket in TIME_WAIT on the server side for
	// every probe interval.
	client := &http.Client{
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext:       (&net.Dialer{Timeout: timeout}).DialContext,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("httpx: probe %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("httpx: probe %s: unhealthy status %s", url, resp.Status)
	}
	return nil
}

// Probe is the whole main-function body for a probe invocation: it resolves the
// listen address from the environment the service itself was configured with,
// runs the probe, and returns the process exit code. 0 means healthy, 1 means
// not — the only two values Docker's healthcheck contract distinguishes.
//
// envAddr is read directly rather than through config.Load because the probe
// must not inherit the server's full requirement set. `api` cannot start
// without SHARPLINE_JWT_SIGNING_KEY, but a probe of a *running* api has no use
// for the signing key, and failing the probe over it would report a healthy
// process as unhealthy.
func Probe(ctx context.Context, envAddr, fallbackAddr, path string, out *os.File) int {
	addr := strings.TrimSpace(os.Getenv(envAddr))
	if addr == "" {
		addr = fallbackAddr
	}

	if err := RunProbe(ctx, addr, path, DefaultProbeTimeout); err != nil {
		// Docker surfaces this in `docker inspect .State.Health.Log`, which is
		// the only diagnostic an operator gets for a distroless container. If
		// even writing it fails there is nothing further to escalate to — the
		// exit code below is the signal that actually matters.
		_, _ = fmt.Fprintf(out, "%v\n", err)
		return 1
	}
	return 0
}

// LoopbackAddr rewrites a net/http listen address into one that can be dialed
// from inside the same network namespace.
//
// ":8080" and "0.0.0.0:8080" both mean "every interface" to a listener, but
// neither is dialable as written — an empty host resolves to nothing and
// "0.0.0.0" is not a destination address. Both become "127.0.0.1:8080". An
// address that already names a concrete host is returned untouched, so a
// service bound to a single interface is still probed on that interface.
func LoopbackAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("%w: listen address is empty", ErrInvalidOptions)
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("%w: listen address %q: %s", ErrInvalidOptions, addr, err)
	}
	if port == "" {
		return "", fmt.Errorf("%w: listen address %q has no port", ErrInvalidOptions, addr)
	}

	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}
