//go:build otel

// Real tracing (M2.4, -tags otel). A dependency-free in-process tracer:
// the repo has no OpenTelemetry SDK in go.mod, so this implements the
// parts the gateway needs — spans with trace/span IDs, parent linkage,
// attributes, events, W3C traceparent inject/extract — and keeps finished
// spans in a bounded in-memory ring for inspection/export. No external
// exporter is wired: exporting to an OTLP endpoint is a follow-up that
// needs a collector address in config, and inventing one here would be
// fake work.
//
// Build with: go build -tags otel ./...
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// maxRecordedSpans bounds the in-memory span ring.
const maxRecordedSpans = 1024

// Tracer records spans in memory when built with -tags otel.
type Tracer struct {
	mu    sync.Mutex
	spans []SpanData
	// exporter, when set by NewConfiguredTracer, also ships finished
	// spans to an OTLP/HTTP collector (bounded queue; never blocks).
	exporter *otlpExporter
}

// NewTracer returns a recording Tracer.
func NewTracer() *Tracer { return &Tracer{} }

// Enabled reports whether tracing records anything. True with the otel tag.
func (t *Tracer) Enabled() bool { return true }

func newTraceID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newSpanID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type otelSpan struct {
	tracer *Tracer
	data   SpanData
	mu     sync.Mutex
	ended  bool
}

func (s *otelSpan) Context() SpanContext {
	return SpanContext{TraceID: s.data.TraceID, SpanID: s.data.SpanID, Sampled: true}
}

func (s *otelSpan) IsRecording() bool { return true }

func (s *otelSpan) SetAttribute(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if s.data.Attributes == nil {
		s.data.Attributes = map[string]string{}
	}
	s.data.Attributes[key] = value
}

func (s *otelSpan) AddEvent(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	s.data.Events = append(s.data.Events, name)
}

func (s *otelSpan) End() {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	s.data.End = time.Now()
	done := s.data
	s.mu.Unlock()
	s.tracer.record(done)
}

func (t *Tracer) record(sd SpanData) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.spans) >= maxRecordedSpans {
		// Ring: drop the oldest span, keep the newest.
		copy(t.spans, t.spans[1:])
		t.spans = t.spans[:len(t.spans)-1]
	}
	// Copy the attributes map so later mutation cannot corrupt the record.
	if sd.Attributes != nil {
		attrs := make(map[string]string, len(sd.Attributes))
		for k, v := range sd.Attributes {
			attrs[k] = v
		}
		sd.Attributes = attrs
	}
	t.spans = append(t.spans, sd)
	if t.exporter != nil {
		// Never block the request path on export pressure.
		t.exporter.enqueue(sd)
	}
}

// StartSpan starts a span named name. Its parent is the span carried by
// ctx (if any); a root span starts a new trace. The returned ctx carries
// the new span, so children started from it link to this span.
func (t *Tracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	parent, _ := SpanContextFrom(ctx)
	sc := SpanContext{TraceID: newTraceID(), SpanID: newSpanID(), Sampled: true}
	kind := "internal"
	var parentID string
	if parent.TraceID != "" {
		sc.TraceID = parent.TraceID
		parentID = parent.SpanID
	}
	if name == "codvian.request" {
		kind = "server"
	}
	span := &otelSpan{
		tracer: t,
		data: SpanData{
			Name:         name,
			Kind:         kind,
			TraceID:      sc.TraceID,
			SpanID:       sc.SpanID,
			ParentSpanID: parentID,
			Start:        time.Now(),
			Attributes:   map[string]string{},
		},
	}
	return contextWithSpanContext(ctx, sc), span
}

// Spans returns a copy of the recorded finished spans, oldest first.
func (t *Tracer) Spans() []SpanData {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]SpanData, len(t.spans))
	copy(out, t.spans)
	return out
}

// Traceparent returns the W3C traceparent header for the span in ctx
// ("00-<trace-id>-<span-id>-01"). Empty when ctx carries no span.
func (t *Tracer) Traceparent(ctx context.Context) string {
	sc, ok := SpanContextFrom(ctx)
	if !ok || sc.TraceID == "" || sc.SpanID == "" {
		return ""
	}
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + sc.TraceID + "-" + sc.SpanID + "-" + flags
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// ExtractTraceparent parses a W3C traceparent header ("00-<32hex>-<16hex>-<flags>")
// into ctx, returning false for malformed headers. The extracted span
// becomes the parent of spans started from the returned ctx.
func (t *Tracer) ExtractTraceparent(ctx context.Context, header string) (context.Context, bool) {
	parts := strings.Split(header, "-")
	if len(parts) != 4 || parts[0] != "00" {
		return ctx, false
	}
	if len(parts[1]) != 32 || !isHex(parts[1]) || len(parts[2]) != 16 || !isHex(parts[2]) {
		return ctx, false
	}
	if len(parts[3]) != 2 || !isHex(parts[3]) {
		return ctx, false
	}
	sc := SpanContext{TraceID: strings.ToLower(parts[1]), SpanID: strings.ToLower(parts[2]), Sampled: parts[3] == "01"}
	return contextWithSpanContext(ctx, sc), true
}
