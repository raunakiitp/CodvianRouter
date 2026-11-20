// Shared tracing types and helpers (M2.4). The Tracer implementation
// itself is build-tagged: trace.go (!otel) is a zero-overhead no-op,
// trace_otel.go (otel) records real spans. Both expose the identical API,
// so callers never branch on the tag.
package observability

import (
	"context"
	"time"
)

// SpanContext identifies a trace and the span within it. IDs are hex, in
// the W3C trace-context shapes (32 hex trace ID, 16 hex span ID).
type SpanContext struct {
	TraceID string
	SpanID  string
	// Sampled mirrors the W3C traceparent flags bit 0.
	Sampled bool
}

// Span is one unit of traced work. Implementations in both builds satisfy
// this interface; the no-op build's methods do nothing.
type Span interface {
	End()
	SetAttribute(key, value string)
	AddEvent(name string)
	Context() SpanContext
	IsRecording() bool
}

// SpanData is a finished span as recorded by the otel-tagged tracer. It is
// the exportable form: name, IDs, parent linkage, timing, attributes and
// events. In the default build Spans() always returns nil.
type SpanData struct {
	Name         string
	Kind         string // "server" for the request root, "internal" otherwise
	TraceID      string
	SpanID       string
	ParentSpanID string
	Start        time.Time
	End          time.Time
	Attributes   map[string]string
	Events       []string
}

// Duration returns the measured span lifetime. Zero for an unfinished span.
func (s SpanData) Duration() time.Duration {
	if s.End.IsZero() || s.Start.IsZero() {
		return 0
	}
	return s.End.Sub(s.Start)
}

type spanContextKey struct{}

// contextWithSpanContext stores sc in ctx for traceparent propagation.
func contextWithSpanContext(ctx context.Context, sc SpanContext) context.Context {
	return context.WithValue(ctx, spanContextKey{}, sc)
}

// SpanContextFrom returns the SpanContext carried by ctx, if any.
func SpanContextFrom(ctx context.Context) (SpanContext, bool) {
	if ctx == nil {
		return SpanContext{}, false
	}
	sc, ok := ctx.Value(spanContextKey{}).(SpanContext)
	return sc, ok
}

// StartRequestSpan starts the root span for one gateway request
// ("codvian.request") and records the request ID on it. Children for the
// request path (auth, route, upstream, stream) are started with
// Tracer.StartSpan on the returned ctx, which makes them children of this
// span in the otel build and no-ops in the default build.
func (t *Tracer) StartRequestSpan(ctx context.Context, requestID string) (context.Context, Span) {
	ctx, span := t.StartSpan(ctx, "codvian.request")
	if requestID != "" {
		span.SetAttribute("request_id", requestID)
	}
	return ctx, span
}
