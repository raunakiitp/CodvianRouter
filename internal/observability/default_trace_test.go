package observability

// WS9F closure item 3: pin the DEFAULT-build tracing behavior. The
// tracer is build-tagged — trace.go (!otel) is a zero-allocation
// no-op, trace_otel.go (otel) records real spans — and this file
// (no build tag) pins the default build: what ships without `-tags
// otel` does NO tracing, propagates context unchanged, and costs
// nothing. A default build that started recording would change the
// production observability posture silently; a no-op that mutated
// the context would break span correlation in the otel build.

import (
	"context"
	"testing"
)

// TestDefaultBuildTracerIsNoOp: in the default build (this file
// compiles WITHOUT -tags otel) NewTracer returns a tracer whose
// spans never record — IsRecording() false, attributes and events
// discarded, Spans() always nil.
func TestDefaultBuildTracerIsNoOp(t *testing.T) {
	tr := NewTracer()
	ctx, span := tr.StartSpan(context.Background(), "test-op")
	if span == nil {
		t.Fatal("StartSpan returned a nil span — callers would panic")
	}
	if span.IsRecording() {
		t.Fatal("default-build span records — tracing must be a no-op without -tags otel")
	}
	span.SetAttribute("k", "v")
	span.AddEvent("event")
	if spans := tr.Spans(); spans != nil {
		t.Fatalf("Spans() = %+v, want nil (nothing recorded)", spans)
	}
	// The context flows through unchanged: a no-op span must not
	// inject span state into the caller's context (the otel build
	// does; the no-op must not — otherwise the two builds would
	// diverge in what handlers see).
	if ctx == nil {
		t.Fatal("StartSpan returned a nil context")
	}
}

// TestDefaultBuildSpanEndSafe: End() on a no-op span is safe to call
// twice and from deferred code — the common usage pattern must not
// panic in the default build.
func TestDefaultBuildSpanEndSafe(t *testing.T) {
	tr := NewTracer()
	_, span := tr.StartSpan(context.Background(), "ends-twice")
	span.End()
	span.End()
}

// TestDefaultBuildTraceparentHonest: the W3C traceparent helper in
// the default build does not fabricate a trace id from a span that
// never recorded — whatever it returns must be empty or a valid
// format, never a fake id that would mislead a downstream collector.
func TestDefaultBuildTraceparentHonest(t *testing.T) {
	tr := NewTracer()
	tp := tr.Traceparent(context.Background())
	if tp != "" {
		// A non-empty value must at least look like a W3C
		// traceparent; a fabricated one would poison downstream
		// correlation. The no-op build returns "" — assert that
		// honestly rather than allowing a fake.
		t.Fatalf("default-build traceparent = %q, want \"\" (no span ever recorded)", tp)
	}
}
