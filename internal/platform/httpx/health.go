// Package httpx provides the operational HTTP surface every Sharpline service
// exposes on its own internal port: GET /healthz (liveness), GET /readyz
// (readiness) and GET /metrics (Prometheus).
//
// It exists so the six cmd/ entrypoints do not each re-implement probe
// semantics, listener timeouts and graceful shutdown. Nothing here is global:
// the logger, the metrics registry and the readiness checkers are all
// constructor-injected (CLAUDE.md §12).
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Route paths. Frozen topology: the proxy publishes /healthz and /readyz
// nowhere and /metrics nowhere — these are reachable only on the service's own
// internal port, by kubelet probes and by the Prometheus scraper.
const (
	PathHealthz = "/healthz"
	PathReadyz  = "/readyz"
	PathMetrics = "/metrics"
)

// Defaults for ServerOptions. Deliberately conservative; a service with
// different needs (stream, whose WebSocket connections must not be cut off by
// a write deadline) overrides them explicitly.
const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 15 * time.Second
	DefaultWriteTimeout      = 30 * time.Second
	DefaultIdleTimeout       = 90 * time.Second
	DefaultReadinessTimeout  = 3 * time.Second
	DefaultShutdownTimeout   = 15 * time.Second
)

// ErrInvalidOptions is returned by NewServer when ServerOptions cannot produce
// a usable server.
var ErrInvalidOptions = errors.New("httpx: invalid server options")

// Checker is the readiness contract, declared here by the consumer rather than
// alongside each dependency (CLAUDE.md §12: "Interfaces are declared by the
// consumer, not the producer. Keep them small."). A Postgres pool, a Redis
// client and a Kafka client each satisfy it without importing this package's
// concerns into theirs.
type Checker interface {
	// Name identifies the dependency in the /readyz payload.
	Name() string
	// Check reports whether the dependency is usable right now. ctx carries
	// the probe's deadline; implementations must honour it.
	Check(ctx context.Context) error
}

// CheckFunc adapts a function to Checker.
type CheckFunc struct {
	CheckerName string
	Fn          func(ctx context.Context) error
}

// Name implements Checker.
func (c CheckFunc) Name() string { return c.CheckerName }

// Check implements Checker.
func (c CheckFunc) Check(ctx context.Context) error {
	if c.Fn == nil {
		return nil
	}
	return c.Fn(ctx)
}

// ServerOptions configures the operational listener. Only Service, Addr and
// Logger are required; every duration falls back to the Default* constant.
type ServerOptions struct {
	// Service is the binary name, reported by /healthz.
	Service string
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// Logger receives lifecycle events. Required — a service that cannot log
	// its own startup is not observable.
	Logger *slog.Logger
	// Registry is the Prometheus registry served at /metrics. When nil a
	// fresh, non-global registry is created with the Go runtime and process
	// collectors registered.
	Registry *prometheus.Registry
	// Checkers are consulted by /readyz. Empty means "ready as soon as the
	// listener is up", which is correct for a service with no dependencies.
	Checkers []Checker
	// PublicPrefix, when non-empty, additionally mounts /healthz and /readyz
	// beneath that prefix — and nothing else.
	//
	// deploy/proxy/Caddyfile forwards /api/* to this service WITHOUT stripping
	// the prefix, so a request the operator writes as `GET /api/healthz`
	// arrives here as `/api/healthz` and misses the root-mounted probe. Setting
	// PublicPrefix = "/api" is what makes the health of the service observable
	// from outside the container network, which matters because the proxy is
	// the only published port (CLAUDE.md §9) and is therefore the only vantage
	// point a reviewer has.
	//
	// PathMetrics is deliberately NOT mirrored. The Caddyfile hard-denies
	// /metrics* at the site root, but that matcher does not cover /api/metrics;
	// mirroring it here would punch a hole straight through that deny rule.
	// Prometheus scrapes the internal port directly and needs nothing public.
	PublicPrefix string

	// Timeouts. Zero means "use the Default* constant"; a negative value means
	// "no deadline". stream passes a negative WriteTimeout once it serves
	// long-lived WebSocket connections, which a write deadline would sever.
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ReadinessTimeout  time.Duration
	ShutdownTimeout   time.Duration
}

// Server is the operational HTTP listener for one service.
type Server struct {
	service          string
	log              *slog.Logger
	mux              *http.ServeMux
	srv              *http.Server
	registry         *prometheus.Registry
	checkers         []Checker
	readinessTimeout time.Duration
	shutdownTimeout  time.Duration
}

// NewServer builds the operational listener. It does not bind a socket; call
// Run for that.
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Service == "" {
		return nil, fmt.Errorf("%w: Service is empty", ErrInvalidOptions)
	}
	if opts.Addr == "" {
		return nil, fmt.Errorf("%w: Addr is empty", ErrInvalidOptions)
	}
	if opts.Logger == nil {
		return nil, fmt.Errorf("%w: Logger is nil", ErrInvalidOptions)
	}

	registry := opts.Registry
	if registry == nil {
		registry = NewRegistry()
	}

	mux := http.NewServeMux()

	s := &Server{
		service:          opts.Service,
		log:              opts.Logger,
		mux:              mux,
		registry:         registry,
		checkers:         append([]Checker(nil), opts.Checkers...),
		readinessTimeout: positiveOrDefault(opts.ReadinessTimeout, DefaultReadinessTimeout),
		shutdownTimeout:  positiveOrDefault(opts.ShutdownTimeout, DefaultShutdownTimeout),
	}

	mux.HandleFunc("GET "+PathHealthz, s.handleHealthz)
	mux.HandleFunc("GET "+PathReadyz, s.handleReadyz)

	// Mirror the two probes under the public prefix. Trailing slashes are
	// trimmed so both "/api" and "/api/" produce "/api/healthz" rather than
	// "/api//healthz", which net/http treats as a distinct, unmatched path.
	if prefix := strings.TrimRight(opts.PublicPrefix, "/"); prefix != "" {
		mux.HandleFunc("GET "+prefix+PathHealthz, s.handleHealthz)
		mux.HandleFunc("GET "+prefix+PathReadyz, s.handleReadyz)
	}

	mux.Handle("GET "+PathMetrics, promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelError),
		ErrorHandling:     promhttp.HTTPErrorOnError,
		EnableOpenMetrics: true,
	}))

	s.srv = &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: orDefault(opts.ReadHeaderTimeout, DefaultReadHeaderTimeout),
		ReadTimeout:       orDefault(opts.ReadTimeout, DefaultReadTimeout),
		WriteTimeout:      orDefault(opts.WriteTimeout, DefaultWriteTimeout),
		IdleTimeout:       orDefault(opts.IdleTimeout, DefaultIdleTimeout),
		ErrorLog:          slog.NewLogLogger(opts.Logger.Handler(), slog.LevelError),
	}

	return s, nil
}

// NewRegistry returns a fresh Prometheus registry carrying the Go runtime and
// process collectors. It is deliberately not the package-global
// prometheus.DefaultRegisterer: no global mutable state (CLAUDE.md §12).
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return reg
}

// Registry exposes the metrics registry so a service can register its own
// collectors before Run.
func (s *Server) Registry() *prometheus.Registry { return s.registry }

// Handle mounts an additional route. Application handlers (the REST API, the
// WebSocket upgrade endpoint) are attached this way.
func (s *Server) Handle(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// Addr reports the configured listen address.
func (s *Server) Addr() string { return s.srv.Addr }

// Run binds the listener and serves until ctx is cancelled, then shuts down
// gracefully within ShutdownTimeout. It returns nil on a clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	serveErr := make(chan error, 1)

	go func() {
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- fmt.Errorf("httpx: listen on %s: %w", s.srv.Addr, err)
			return
		}
		serveErr <- nil
	}()

	s.log.Info("operational listener started",
		slog.String("addr", s.srv.Addr),
		slog.String("healthz", PathHealthz),
		slog.String("readyz", PathReadyz),
		slog.String("metrics", PathMetrics),
	)

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}

	s.log.Info("shutdown signal received, draining",
		slog.String("timeout", s.shutdownTimeout.String()))

	// Detach from the cancelled parent so the drain gets its full budget.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("httpx: graceful shutdown of %s: %w", s.service, err)
	}
	return <-serveErr
}

// healthPayload is the /healthz and /readyz response body.
type healthPayload struct {
	Status  string                 `json:"status"`
	Service string                 `json:"service"`
	Checks  map[string]checkResult `json:"checks,omitempty"`
}

type checkResult struct {
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// handleHealthz is liveness: the process is running and its scheduler is
// responsive. It must not consult dependencies — a liveness probe that fails
// because Postgres is down restarts a healthy pod for no reason.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, healthPayload{Status: "ok", Service: s.service})
}

// handleReadyz is readiness: every injected dependency answered inside the
// probe deadline. With no checkers registered the service is ready once it is
// listening, which is the honest answer for a service that depends on nothing.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.readinessTimeout)
	defer cancel()

	payload := healthPayload{Status: "ready", Service: s.service}
	status := http.StatusOK

	if len(s.checkers) > 0 {
		payload.Checks = make(map[string]checkResult, len(s.checkers))
		for _, c := range s.checkers {
			if err := c.Check(ctx); err != nil {
				payload.Status = "not ready"
				status = http.StatusServiceUnavailable
				payload.Checks[c.Name()] = checkResult{Status: "down", Error: err.Error()}
				continue
			}
			payload.Checks[c.Name()] = checkResult{Status: "up"}
		}
	}

	if status != http.StatusOK {
		s.log.Warn("readiness probe failed", slog.Any("checks", payload.Checks))
	}
	s.writeJSON(w, status, payload)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload healthPayload) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written; all that is left is to say so.
		s.log.Error("writing probe response", slog.String("error", err.Error()))
	}
}

// orDefault resolves a zero-valued duration to its default. A negative value is
// an explicit "disable this deadline" and resolves to zero, which is how
// net/http spells "no timeout".
func orDefault(v, fallback time.Duration) time.Duration {
	switch {
	case v < 0:
		return 0
	case v == 0:
		return fallback
	default:
		return v
	}
}

// positiveOrDefault is orDefault for the two budgets that must never be
// disabled: a zero readiness deadline expires instantly and a zero drain budget
// makes graceful shutdown pointless.
func positiveOrDefault(v, fallback time.Duration) time.Duration {
	if v <= 0 {
		return fallback
	}
	return v
}
