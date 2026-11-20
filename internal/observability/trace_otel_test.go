//go:build otel

// Acceptance tests for M2.4 — OpenTelemetry traces WITH the otel build tag.
// Run with: go test -tags otel ./internal/observability/
package observability

import (
	"context"
	"strings"
	"testing"
)

func TestTracingEnabledWithOtelTag(t *testing.T) {
	tr := NewTracer()
	if !tr.Enabled() {
		t.Fatal("tracer Enabled() = false with -tags otel, want true")
	}
	ctx, span := tr.StartSpan(context.Background(), "codvian.request")
	if !span.IsRecording() {
		t.Fatal("otel span IsRecording() = false, want true")
	}
	span.SetAttribute("provider", "openai")
	span.AddEvent("auth.ok")
	span.End()
	_ = ctx
	spans := tr.Spans()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	if spans[0].Name != "codvian.request" || spans[0].Attributes["provider"] != "openai" {
		t.Fatalf("recorded span wrong: %+v", spans[0])
	}
	if len(spans[0].Events) != 1 || spans[0].Events[0] != "auth.ok" {
		t.Fatalf("recorded events wrong: %+v", spans[0].Events)
	}
	if spans[0].TraceID == "" || spans[0].SpanID == "" {
		t.Fatalf("span lacks IDs: %+v", spans[0])
	}
	if spans[0].End.IsZero() || spans[0].Start.IsZero() {
		t.Fatalf("span lacks timestamps: %+v", spans[0])
	}
}

func TestRequestPathSpanTree(t *testing.T) {
	tr := NewTracer()
	reqCtx, req := tr.StartRequestSpan(context.Background(), "req-9")
	_, auth := tr.StartSpan(reqCtx, "auth")
	auth.End()
	_, route := tr.StartSpan(reqCtx, "route")
	route.End()
	_, upstream := tr.StartSpan(reqCtx, "upstream")
	upstream.End()
	_, stream := tr.StartSpan(reqCtx, "stream")
	stream.End()
	req.End()

	spans := tr.Spans()
	if len(spans) != 5 {
		t.Fatalf("recorded %d spans, want 5 (request + auth + route + upstream + stream)", len(spans))
	}
	byName := map[string]SpanData{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	// All four children share the request span's trace ID and point at it
	// as their parent — the trace is one connected tree, not five orphans.
	for _, name := range []string{"auth", "route", "upstream", "stream"} {
		child := byName[name]
		if child.TraceID != byName["codvian.request"].TraceID {
			t.Fatalf("%s trace %q != request trace %q", name, child.TraceID, byName["codvian.request"].TraceID)
		}
		if child.ParentSpanID != byName["codvian.request"].SpanID {
			t.Fatalf("%s parent %q != request span %q", name, child.ParentSpanID, byName["codvian.request"].SpanID)
		}
	}
}

func TestTraceparentRoundTrip(t *testing.T) {
	tr := NewTracer()
	ctx, span := tr.StartSpan(context.Background(), "codvian.request")
	defer span.End()
	tp := tr.Traceparent(ctx)
	if tp == "" {
		t.Fatal("Traceparent() empty with otel tag")
	}
	parts := strings.Split(tp, "-")
	if len(parts) != 4 || parts[0] != "00" || len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Fatalf("traceparent %q is not W3C-shaped 00-<32hex>-<16hex>-<flags>", tp)
	}
	// Extracting the header we just emitted must recover the same trace.
	ctx2, ok := tr.ExtractTraceparent(context.Background(), tp)
	if !ok {
		t.Fatalf("ExtractTraceparent(%q) failed", tp)
	}
	if got := tr.Traceparent(ctx2); !strings.HasPrefix(got, "00-"+parts[1]+"-") {
		t.Fatalf("extracted traceparent %q does not carry trace %s", got, parts[1])
	}
}
