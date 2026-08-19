package middleware

import (
	"net/http"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the instrumentation scope every span from this package carries.
const tracerName = "github.com/anpl1623/sharpline/internal/httpapi/middleware"

// Trace continues the incoming trace and starts the server span for this
// request.
//
// # Why this is written here rather than taken from otelhttp
//
// go.opentelemetry.io/contrib/.../otelhttp would do most of this, and it also
// emits its own metrics through the OTel metric SDK. This repository exports
// Prometheus metrics directly from a registry it owns (internal/platform/httpx
// serves it, internal/platform/{postgres,kafka,redis} register on it), so
// otelhttp would add a second, parallel metrics pipeline reporting the same
// requests under different names — which is how a dashboard ends up with two
// request-rate panels that disagree. Doing the span by hand keeps one metrics
// story and matches internal/platform/kafka, which propagates trace context
// itself for the same reason.
//
// # The propagator default is not otel.GetTextMapPropagator()
//
// The OTel global propagator is a NO-OP until some entrypoint calls
// otel.SetTextMapPropagator, and no cmd/ entrypoint does. Defaulting to the
// global would produce spans that look correct in Jaeger and are silently
// detached from the ingest -> pricer -> stream trace they belong to, which is
// worse than no tracing because it is not visibly broken. So the default is a
// concrete propagation.TraceContext — the same decision, for the same reason, as
// internal/platform/kafka/otel.go.
//
// # The span name is set twice, deliberately
//
// A span must exist before the router runs, but the route pattern is only known
// after it. Naming the span "GET" up front and renaming it to "GET /api/v1/
// events/{id}" once Route resolves gives a correctly named span without a
// high-cardinality span name (the raw path, which contains ids) ever existing.
func Trace(tp trace.TracerProvider, prop propagation.TextMapPropagator) Middleware {
	if prop == nil {
		prop = propagation.TraceContext{}
	}
	tracer := tp.Tracer(tracerName)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := prop.Extract(r.Context(), propagation.HeaderCarrier(r.Header))

			// The route is usually already resolved by ResolveRoute, which runs
			// above this. When it is, the span is named correctly from the
			// start; when it is not (a router that resolves during dispatch),
			// the name is corrected below.
			name := r.Method
			if route := RouteFrom(ctx); route != "" {
				name = r.Method + " " + route
			}

			// Only non-secret, bounded request facts. Never a header, never the
			// query string, never a body. url.path is included because a trace
			// is an operator tool and the exact path is what makes one usable;
			// the query string is excluded because a badly written client can
			// put a token in it and a span is exported off-box.
			ctx, span := tracer.Start(ctx, name,
				trace.WithSpanKind(trace.SpanKindServer),
				trace.WithAttributes(
					attribute.String("http.request.method", r.Method),
					attribute.String("url.path", r.URL.Path),
					attribute.String("url.scheme", schemeOf(r)),
					attribute.String("network.protocol.version", r.Proto),
					attribute.String("server.address", r.Host),
				),
			)
			defer span.End()

			rec := ensureRecorder(w)

			next.ServeHTTP(rec, r.WithContext(ctx))

			if route := RouteFrom(ctx); route != "" {
				span.SetName(r.Method + " " + route)
				span.SetAttributes(attribute.String("http.route", route))
			}
			span.SetAttributes(attribute.Int("http.response.status_code", rec.status))

			// OTel's HTTP convention: a server span is an error only for 5xx.
			// A 401 or a 429 is the system working, and marking it as an error
			// makes the error rate in Jaeger meaningless.
			if rec.status >= 500 {
				span.SetStatus(codes.Error, statusClass(rec.status))
			}
		})
	}
}

// schemeOf reports the scheme the CLIENT used, which is https at the proxy even
// though this hop is plain HTTP. r.TLS is nil here for every request, so the
// only signal is X-Forwarded-Proto — and it is used for a span attribute only,
// never for a security decision, which is why it needs no trusted-proxy check.
func schemeOf(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-Proto"); v == "https" || v == "http" {
		return v
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
