package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// The carrier, the propagator default, and the span helpers.
//
// The END-TO-END property — that a trace started in a producer is picked up by a
// consumer on the other side of a real broker — is in
// test/integration/kafka_test.go and has to be, because a record's headers only
// prove anything after a broker has stored and returned them. What is here is
// the mechanism that test depends on, checked where a failure names the exact
// piece rather than reporting "the trace ids differ".

// TestDefaultPropagatorIsConcreteRatherThanTheGlobal pins the deliberate
// divergence otel.go documents.
//
// otel.GetTextMapPropagator() returns a NO-OP until some entrypoint calls
// otel.SetTextMapPropagator, and no cmd/ entrypoint does that yet. Depending on
// the global would mean the headers silently carried nothing and every trace
// stopped at the producer — a failure indistinguishable from tracing being
// switched off, because the spans would still be emitted.
func TestDefaultPropagatorIsConcreteRatherThanTheGlobal(t *testing.T) {
	t.Parallel()

	prop := defaultPropagator()
	if _, ok := prop.(propagation.TraceContext); !ok {
		t.Fatalf("defaultPropagator() = %T, want propagation.TraceContext", prop)
	}

	// It carries traceparent and nothing else: baggage is deliberately not
	// propagated, because what user-derived data may cross a service boundary is
	// an ADR-shaped decision rather than a default.
	fields := prop.Fields()
	for _, f := range fields {
		if f == "baggage" {
			t.Error("the default propagator carries baggage; that is an ADR-shaped decision, not a default")
		}
	}

	// And an options value with no propagator resolves to the same thing.
	if _, ok := (ClientOptions{}).propagator().(propagation.TraceContext); !ok {
		t.Error("ClientOptions with no Propagator does not resolve to W3C trace context")
	}
}

// TestHeaderCarrierReadsWritesAndReplaces covers the adapter both sides of the
// bus use.
//
// Set REPLACES rather than appends, and that is the load-bearing part: a record
// with two traceparent headers is ambiguous, and different consumers would pick
// different ones — which produces a trace that is intermittently broken rather
// than reliably broken, the worse of the two failures.
func TestHeaderCarrierReadsWritesAndReplaces(t *testing.T) {
	t.Parallel()

	rec := &kgo.Record{Headers: []kgo.RecordHeader{
		{Key: HeaderProducer, Value: []byte("ingest")},
	}}
	carrier := headerCarrier{headers: &rec.Headers}

	if got := carrier.Get(HeaderProducer); got != "ingest" {
		t.Errorf("Get(%q) = %q, want %q", HeaderProducer, got, "ingest")
	}
	if got := carrier.Get("absent"); got != "" {
		t.Errorf("Get on a missing key = %q, want the empty string", got)
	}

	carrier.Set("traceparent", "first")
	carrier.Set("traceparent", "second")

	if got := carrier.Get("traceparent"); got != "second" {
		t.Errorf("Get(traceparent) = %q, want the replaced value %q", got, "second")
	}

	var traceparents int
	for _, h := range rec.Headers {
		if h.Key == "traceparent" {
			traceparents++
		}
	}
	if traceparents != 1 {
		t.Errorf("the record carries %d traceparent headers, want exactly 1; two are ambiguous "+
			"and different consumers would join different traces", traceparents)
	}

	keys := carrier.Keys()
	if len(keys) != 2 || keys[0] != HeaderProducer || keys[1] != "traceparent" {
		t.Errorf("Keys() = %v, want the record's headers in order", keys)
	}
}

// TestInjectAndExtractRoundTripTheSpanContext covers the two functions the
// producer and the consumer actually call.
//
// This is deliberately NOT done through a carrier the test defines. otel.go's
// headerCarrier is the thing that has to work; a test with its own carrier
// proves the propagator works and says nothing about this package.
func TestInjectAndExtractRoundTripTheSpanContext(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	prop := defaultPropagator()

	ctx, span := tp.Tracer("test").Start(t.Context(), "publish")
	span.End()
	local := span.SpanContext()
	if !local.IsValid() {
		t.Fatal("the test's own span context is invalid")
	}

	rec := &kgo.Record{Headers: []kgo.RecordHeader{{Key: HeaderProducer, Value: []byte("ingest")}}}
	injectTrace(ctx, prop, rec)

	if flattenHeaders(rec.Headers)["traceparent"] == "" {
		t.Fatalf("injectTrace wrote no traceparent; headers are %v", rec.Headers)
	}

	remote := trace.SpanContextFromContext(extractTrace(context.Background(), prop, rec))
	if !remote.IsValid() {
		t.Fatal("extractTrace produced no span context; the trace stops at the producer")
	}
	if remote.TraceID() != local.TraceID() {
		t.Errorf("trace id = %s, want %s", remote.TraceID(), local.TraceID())
	}
	if remote.SpanID() != local.SpanID() {
		t.Errorf("parent span id = %s, want %s", remote.SpanID(), local.SpanID())
	}
	if !remote.IsRemote() {
		t.Error("the extracted span context is not marked remote; it was not reconstructed from the record")
	}
	if !remote.IsSampled() {
		t.Error("the sampling decision did not survive; a sampled producer trace with an unsampled " +
			"consumer half is a trace that ends mid-pipeline")
	}
}

// TestExtractWithNoTracingHeadersLeavesTheContextAlone covers the case a record
// produced by something that does not trace takes.
//
// The consumer span then becomes a ROOT, which is correct: inventing a parent
// would attach real work to a trace that does not exist.
func TestExtractWithNoTracingHeadersLeavesTheContextAlone(t *testing.T) {
	t.Parallel()

	rec := &kgo.Record{Headers: []kgo.RecordHeader{{Key: HeaderProducer, Value: []byte("ingest")}}}
	got := extractTrace(t.Context(), defaultPropagator(), rec)

	if sc := trace.SpanContextFromContext(got); sc.IsValid() {
		t.Errorf("extractTrace invented a span context %s from a record with no tracing headers", sc.TraceID())
	}
}

// TestBaseSpanAttrsFollowTheMessagingConventions pins the three attributes every
// bus span carries.
//
// They are written as literals rather than taken from a semconv/vN package for
// the reason otel.go states: a semconv version bump must not silently rename an
// attribute a saved Jaeger query depends on. This is what makes that promise
// checkable.
func TestBaseSpanAttrsFollowTheMessagingConventions(t *testing.T) {
	t.Parallel()

	attrs := baseSpanAttrs(TopicOddsNormalized, operationPublish)
	got := map[string]string{}
	for _, kv := range attrs {
		got[string(kv.Key)] = kv.Value.String()
	}

	for key, want := range map[string]string{
		"messaging.system":           "kafka",
		"messaging.destination.name": TopicOddsNormalized,
		"messaging.operation.name":   "publish",
	} {
		if got[key] != want {
			t.Errorf("attribute %q = %q, want %q", key, got[key], want)
		}
	}
	if len(attrs) != 3 {
		t.Errorf("baseSpanAttrs returned %d attributes, want 3", len(attrs))
	}
}

// TestRecordSpanErrorMarksTheSpanOnlyOnFailure covers the helper that keeps the
// producer, the consumer and the snapshot reader from drifting on how a failure
// is reported.
func TestRecordSpanErrorMarksTheSpanOnlyOnFailure(t *testing.T) {
	t.Parallel()

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	tracer := tp.Tracer("test")

	_, ok := tracer.Start(t.Context(), "ok")
	recordSpanError(ok, nil)
	ok.End()

	_, bad := tracer.Start(t.Context(), "bad")
	recordSpanError(bad, errors.New("produce refused"))
	bad.End()

	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(spans))
	}
	for _, s := range spans {
		switch s.Name {
		case "ok":
			if s.Status.Code != codes.Unset {
				t.Errorf("a successful span has status %v, want unset", s.Status.Code)
			}
			if len(s.Events) != 0 {
				t.Errorf("a successful span recorded %d events, want none", len(s.Events))
			}
		case "bad":
			if s.Status.Code != codes.Error {
				t.Errorf("a failed span has status %v, want error", s.Status.Code)
			}
			if s.Status.Description != "produce refused" {
				t.Errorf("status description = %q, want the error text", s.Status.Description)
			}
			if len(s.Events) == 0 {
				t.Error("a failed span recorded no exception event")
			}
		}
	}
}
