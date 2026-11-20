//go:build !otel

// Acceptance tests for M2.4 — OpenTelemetry traces OFF by default.
// This file has NO build tag: it runs in the default build and proves the
// off-by-default claim. The with-tag behaviour is in trace_otel_test.go.
package observability

import (
	"context"
	"testing"
)

func TestTracingDisabledByDefault(t *testing.T) {
	tr := NewTracer()
	if tr.Enabled() {
		t.Fatal("tracer Enabled() = true in the default build, want false (off by default)")
	}
	ctx, span := tr.StartSpan(context.Background(), "codvian.request")
	if span.IsRecording() {
		t.Fatal("default-build span IsRecording() = true, want false")
	}
	span.SetAttribute("provider", "openai")
	span.AddEvent("auth.ok")
	span.End()
	_ = ctx
	if got := tr.Spans(); len(got) != 0 {
		t.Fatalf("default build recorded %d spans, want 0", len(got))
	}
	if tp := tr.Traceparent(ctx); tp != "" {
		t.Fatalf("default build Traceparent() = %q, want empty", tp)
	}
}

func TestRequestPathSpansAreNoopWhenTracingOff(t *testing.T) {
	tr := NewTracer()
	ctx := context.Background()
	// The request-path helpers (auth -> route -> upstream -> stream) must
	// exist and be safe to call in the default build, recording nothing.
	ctx, req := tr.StartRequestSpan(ctx, "req-1")
	_, auth := tr.StartSpan(ctx, "auth")
	_, route := tr.StartSpan(ctx, "route")
	_, upstream := tr.StartSpan(ctx, "upstream")
	_, stream := tr.StartSpan(ctx, "stream")
	for _, s := range []Span{req, auth, route, upstream, stream} {
		if s.IsRecording() {
			t.Fatal("a request-path span is recording in the default build")
		}
		s.End()
	}
	if got := tr.Spans(); len(got) != 0 {
		t.Fatalf("default build recorded %d spans, want 0", len(got))
	}
}
