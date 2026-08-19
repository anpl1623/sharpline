package middleware

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// ErrInvalidOptions is returned when options cannot produce a usable chain.
// Callers match it with errors.Is, following the precedent set by
// internal/platform/{config,httpx,postgres,kafka,redis}.
var ErrInvalidOptions = errors.New("middleware: invalid options")

func invalidOption(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidOptions, fmt.Sprintf(format, args...))
}

// StackOptions configures the canonical chain. Only Service and Logger are
// required; everything else degrades to a documented default or to that link
// being omitted.
//
// A nil dependency omits its link rather than failing. That is what makes the
// chain usable before the whole phase exists — an api that has no Authenticator
// yet still gets request ids, metrics, logging and panic recovery — and it is
// why NewStack logs exactly which links it assembled, so "rate limiting is off"
// is a line in the startup log rather than a discovery.
type StackOptions struct {
	// Service is the binary name. Required.
	Service string

	// Logger is the base logger. Required.
	Logger *slog.Logger

	// Registry is where the HTTP collectors are registered. nil registers
	// nothing, which is correct for a unit test; a service passes
	// httpx.Server.Registry().
	Registry prometheus.Registerer

	// TracerProvider supplies the tracer. nil uses the OTel global provider,
	// which is a no-op until a cmd/ entrypoint installs a real one.
	TracerProvider trace.TracerProvider

	// Propagator extracts inbound trace context. nil uses W3C trace context
	// DIRECTLY rather than the OTel global — see Trace for why the global is
	// the wrong default.
	Propagator propagation.TextMapPropagator

	// RouteFunc resolves the route pattern for metric labels and span names.
	// nil means every request is labelled "unmatched", which is honest but
	// useless; pass MuxRouteFunc(mux). See RouteFunc.
	RouteFunc RouteFunc

	// TrustedProxies is the set of peers whose forwarding headers are believed.
	// EMPTY MEANS NO HEADER IS EVER BELIEVED, which is the safe default and the
	// wrong one behind a proxy — see TrustedProxies for what to put in it and
	// what happens if you leave it empty behind Caddy.
	TrustedProxies TrustedProxies

	// RequestTimeout bounds one request. Zero means DefaultRequestTimeout;
	// negative disables the deadline.
	RequestTimeout time.Duration

	// MaxRequestBytes caps the request body. Zero means
	// DefaultMaxRequestBytes.
	MaxRequestBytes int64

	// CORS configures cross-origin access. Its zero value denies every
	// cross-origin request, which is correct for this deployment — see
	// CORSOptions.
	CORS CORSOptions

	// IPLimiter and UserLimiter are the two controls CLAUDE.md §6 requires.
	// Either may be nil, which omits that link.
	IPLimiter   Limiter
	UserLimiter Limiter

	// RateLimitFailClosed makes an undecidable rate-limit check a 429 instead
	// of admitting the request. False (fail open) is the default; see
	// RateLimitOptions.FailClosed for the reasoning.
	RateLimitFailClosed bool

	// RateLimitExempt skips both limiters for a request. Optional.
	RateLimitExempt func(*http.Request) bool

	// Authenticator verifies bearer tokens. nil omits authentication entirely,
	// which means IdentityFrom never reports an identity and RequireIdentity
	// rejects everything — a deliberate fail-closed shape for a partially wired
	// service.
	Authenticator Authenticator

	// AuthCookieName additionally accepts the token from a cookie. Empty by
	// default; see AuthOptions.CookieName for the CSRF obligation that comes
	// with setting it.
	AuthCookieName string

	// ErrorWriter renders every error this chain produces. nil uses
	// WriteProblem. internal/httpapi passes its own so that a 429 from the
	// limiter and a 400 from a handler have the same envelope.
	ErrorWriter ErrorWriter
}

// Stack is the assembled chain.
type Stack struct {
	middlewares []Middleware
	metrics     *Metrics
	errorWriter ErrorWriter
}

// NewStack assembles the canonical chain. The order and the reasoning behind
// each position are in the package doc; this function is the single place it is
// expressed in code.
func NewStack(opts StackOptions) (*Stack, error) {
	switch {
	case opts.Service == "":
		return nil, invalidOption("Service is empty")
	case opts.Logger == nil:
		return nil, invalidOption("Logger is nil")
	}

	metrics, err := NewMetrics(opts.Registry)
	if err != nil {
		return nil, err
	}

	tp := opts.TracerProvider
	if tp == nil {
		tp = otel.GetTracerProvider()
	}

	ew := opts.ErrorWriter
	if ew == nil {
		ew = WriteProblem
	}

	timeout := opts.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}

	maxBytes := opts.MaxRequestBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxRequestBytes
	}

	mw := []Middleware{
		RequestID(),
		ResolveRoute(opts.RouteFunc),
		ClientAddr(opts.TrustedProxies),
		Trace(tp, opts.Propagator),
		Correlate(opts.Logger),
		metrics.Observe(),
		AccessLog(opts.Logger),
		Recover(opts.Logger, metrics, ew),
		SecurityHeaders(),
		CORS(opts.CORS),
	}

	if opts.IPLimiter != nil {
		ipLimit, err := RateLimit(RateLimitOptions{
			Limiter:     opts.IPLimiter,
			Subject:     IPSubject,
			Logger:      opts.Logger,
			Metrics:     metrics,
			ErrorWriter: ew,
			FailClosed:  opts.RateLimitFailClosed,
			Exempt:      opts.RateLimitExempt,
		})
		if err != nil {
			return nil, err
		}
		mw = append(mw, ipLimit)
	}

	mw = append(mw,
		MaxBytes(maxBytes, ew),
		Timeout(timeout, metrics),
	)

	if opts.Authenticator != nil {
		auth, err := Authenticate(AuthOptions{
			Authenticator: opts.Authenticator,
			Metrics:       metrics,
			ErrorWriter:   ew,
			CookieName:    opts.AuthCookieName,
		})
		if err != nil {
			return nil, err
		}
		mw = append(mw, auth)
	}

	if opts.UserLimiter != nil {
		userLimit, err := RateLimit(RateLimitOptions{
			Limiter:     opts.UserLimiter,
			Subject:     UserSubject,
			Logger:      opts.Logger,
			Metrics:     metrics,
			ErrorWriter: ew,
			FailClosed:  opts.RateLimitFailClosed,
			Exempt:      opts.RateLimitExempt,
		})
		if err != nil {
			return nil, err
		}
		mw = append(mw, userLimit)
	}

	// The startup line names every link that is and is not present. A chain
	// silently missing its rate limiter because a constructor returned nil is
	// the failure this exists to make impossible to miss.
	opts.Logger.Info("http middleware chain assembled",
		slog.String("service", opts.Service),
		slog.Int("links", len(mw)),
		slog.Bool("route_resolution", opts.RouteFunc != nil),
		slog.Int("trusted_proxies", len(opts.TrustedProxies)),
		slog.Bool("authentication", opts.Authenticator != nil),
		slog.Bool("auth_cookie", opts.AuthCookieName != ""),
		slog.Bool("rate_limit_ip", opts.IPLimiter != nil),
		slog.Bool("rate_limit_user", opts.UserLimiter != nil),
		slog.Bool("rate_limit_fail_closed", opts.RateLimitFailClosed),
		slog.Int("cors_allowed_origins", len(opts.CORS.AllowedOrigins)),
		slog.String("request_timeout", timeout.String()),
		slog.Int64("max_request_bytes", maxBytes),
	)

	return &Stack{middlewares: mw, metrics: metrics, errorWriter: ew}, nil
}

// Then wraps h in the chain.
func (s *Stack) Then(h http.Handler) http.Handler { return Chain(h, s.middlewares...) }

// Middlewares returns the assembled links, outermost first, so a caller can
// mount a subset or interleave its own.
func (s *Stack) Middlewares() []Middleware {
	out := make([]Middleware, len(s.middlewares))
	copy(out, s.middlewares)
	return out
}

// Metrics exposes the collector set so a route group adding RequireIdentity can
// record its rejections under the same series as the chain's.
func (s *Stack) Metrics() *Metrics { return s.metrics }

// ErrorWriter exposes the resolved error writer, so a handler renders its errors
// the same way the chain renders its own.
func (s *Stack) ErrorWriter() ErrorWriter { return s.errorWriter }

// RequireIdentity is the per-route-group authentication gate, bound to this
// stack's metrics and error writer.
func (s *Stack) RequireIdentity() Middleware {
	return RequireIdentity(s.metrics, s.errorWriter)
}
