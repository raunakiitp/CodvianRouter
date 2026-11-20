//go:build !otel

// No-op tracing (M2.4, default build). Traces are OFF by default and cost
// nothing when off: StartSpan returns the caller's ctx unchanged and a
// zero-size no-op span — no allocation, no I/O, no context storage, no
// recorded data. Callers use the same API in both builds.
package observability

import "context"

type noopSpan struct{}

func (noopSpan) End()                        {}
func (noopSpan) SetAttribute(string, string) {}
func (noopSpan) AddEvent(string)             {}
func (noopSpan) Context() SpanContext        { return SpanContext{} }
func (noopSpan) IsRecording() bool           { return false }

// Tracer is the no-op tracer for the default build.
type Tracer struct{}

// NewTracer returns a Tracer. In the default build it never records.
func NewTracer() *Tracer { return &Tracer{} }

// Enabled reports whether tracing records anything. Always false without
// the otel build tag.
func (t *Tracer) Enabled() bool { return false }

// StartSpan returns ctx unchanged and a non-recording span.
func (t *Tracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	return ctx, noopSpan{}
}

// Spans returns the recorded spans. Always nil in the default build.
func (t *Tracer) Spans() []SpanData { return nil }

// Traceparent returns the W3C traceparent header for the span in ctx.
// Always empty in the default build: there is no trace to propagate.
func (t *Tracer) Traceparent(ctx context.Context) string { return "" }

// ExtractTraceparent parses a W3C traceparent header into ctx. Always
// reports failure in the default build: traces are off.
func (t *Tracer) ExtractTraceparent(ctx context.Context, header string) (context.Context, bool) {
	return ctx, false
}
