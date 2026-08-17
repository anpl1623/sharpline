// OpenTelemetry: spans on both sides of the bus, and trace context carried
// through the record headers.
//
// CLAUDE.md §9 asks for "traces spanning ingest → pricer → stream so a single
// odds update can be followed end to end". That is only achievable if the
// context crosses the bus, which means it has to be serialised into the record.
// A producer span with no matching consumer span is a trace that stops at the
// first hop, which is worse than no instrumentation because it looks like
// instrumentation.
//
// # The consumer span is a CHILD, not a link
//
// Both are defensible and the trade is real. A LINK keeps each service's traces
// short and independent, which is what a high-fan-out queue usually wants. A
// CHILD produces one trace covering the whole traversal, which is the only shape
// in which "follow one odds update end to end" is a single click in Jaeger.
//
// Child is chosen because the pipeline is four hops deep and not fanned out:
// one provider observation becomes one normalized record becomes one computed
// price becomes N socket writes. The trace stays small enough to read. If phase
// 6's fanout ever attaches a span per subscriber, that hop should switch to
// links — a trace with ten thousand children is not a trace anybody opens.
//
// # The propagator is explicit, not global
//
// otel.GetTextMapPropagator() returns a no-op until some entrypoint calls
// otel.SetTextMapPropagator, and no cmd/ entrypoint does that yet. Depending on
// the global would mean the headers silently carried nothing and every trace
// stopped at the producer — a failure indistinguishable from tracing being
// switched off. So the default here is a concrete propagation.TraceContext.
// This is a deliberate divergence from internal/platform/postgres, which does
// take the global TracerProvider; the difference is that a no-op tracer
// provider is a visible absence of spans, whereas a no-op propagator produces
// spans that merely fail to join up.
package kafka

import (
	"context"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracerName identifies this instrumentation library in the trace. It is the
// name under which the bus hops appear in Jaeger.
const tracerName = "github.com/anpl1623/sharpline/internal/platform/kafka"

// Span attribute keys, following the OpenTelemetry messaging semantic
// conventions. Written as literals rather than imported from a semconv/vN
// package for the same reason internal/platform/postgres does it: a semconv
// version bump must not silently rename an attribute a saved Jaeger query
// depends on.
const (
	attrMessagingSystem      = "messaging.system"
	attrMessagingDestination = "messaging.destination.name"
	attrMessagingOperation   = "messaging.operation.name"
	attrMessagingKey         = "messaging.kafka.message.key"
	attrMessagingPartition   = "messaging.destination.partition.id"
	attrMessagingOffset      = "messaging.kafka.offset"
	attrMessagingBodySize    = "messaging.message.body.size"
	attrMessagingBatchSize   = "messaging.batch.message_count"
	attrMessagingGroup       = "messaging.consumer.group.name"

	// Sharpline-specific attributes. Prefixed so they cannot collide with a
	// future semantic convention.
	attrEnvelopeVersion = "sharpline.envelope.version"
	attrMessageType     = "sharpline.message.type"
	attrTombstone       = "sharpline.tombstone"
	attrTombstoneReason = "sharpline.tombstone.reason"

	messagingSystemKafka = "kafka"

	operationPublish   = "publish"
	operationTombstone = "tombstone"
	operationProcess   = "process"
	operationSnapshot  = "snapshot"
)

// headerCarrier adapts a record's headers to propagation.TextMapCarrier.
//
// It holds a pointer to the slice so that Set can append, which is what the
// producer needs: the propagator adds traceparent (and possibly tracestate) to
// headers that already carry the envelope's descriptive keys.
type headerCarrier struct {
	headers *[]kgo.RecordHeader
}

// Get implements propagation.TextMapCarrier.
func (c headerCarrier) Get(key string) string {
	for _, h := range *c.headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// Set implements propagation.TextMapCarrier. It replaces an existing header
// rather than appending a duplicate, because a record with two traceparent
// headers is ambiguous and different consumers would pick different ones.
func (c headerCarrier) Set(key, value string) {
	for i := range *c.headers {
		if (*c.headers)[i].Key == key {
			(*c.headers)[i].Value = []byte(value)
			return
		}
	}
	*c.headers = append(*c.headers, kgo.RecordHeader{Key: key, Value: []byte(value)})
}

// Keys implements propagation.TextMapCarrier.
func (c headerCarrier) Keys() []string {
	keys := make([]string, 0, len(*c.headers))
	for _, h := range *c.headers {
		keys = append(keys, h.Key)
	}
	return keys
}

// defaultPropagator is what an Options that names no propagator gets. W3C trace
// context only: baggage is not propagated, because what user-derived data may
// cross a service boundary is an ADR-shaped decision, not a default.
func defaultPropagator() propagation.TextMapPropagator { return propagation.TraceContext{} }

// injectTrace writes the current span context into the record's headers.
func injectTrace(ctx context.Context, prop propagation.TextMapPropagator, r *kgo.Record) {
	prop.Inject(ctx, headerCarrier{headers: &r.Headers})
}

// extractTrace returns a context carrying the remote span context the record's
// headers describe. With no tracing headers present it returns ctx unchanged,
// and the consumer's span becomes a root — correct for a record produced by
// something that does not trace.
func extractTrace(ctx context.Context, prop propagation.TextMapPropagator, r *kgo.Record) context.Context {
	return prop.Extract(ctx, headerCarrier{headers: &r.Headers})
}

// baseSpanAttrs are the attributes every bus span carries.
func baseSpanAttrs(topic, operation string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(attrMessagingSystem, messagingSystemKafka),
		attribute.String(attrMessagingDestination, topic),
		attribute.String(attrMessagingOperation, operation),
	}
}

// recordSpanError marks a span failed. Kept in one place so the producer, the
// consumer and the snapshot reader cannot drift on how a failure is reported.
func recordSpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
