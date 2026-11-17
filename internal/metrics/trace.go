package metrics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Tracing in this package is a lightweight local JSON trace log.
//
// It is NOT the OpenTelemetry SDK: it emits no OTLP, talks to no
// collector, and has no exporter, sampler or propagator beyond carrying a
// trace ID through context.Context. Each finished span writes exactly one
// JSON line to the configured writer (io.Discard by default) with
// OTel-style field names: trace_id, span_id, name, start_ms,
// duration_ms and attrs. It never writes request/response content —
// only the attributes the caller explicitly sets via SetAttr.
//
// Limitations: spans are not exported anywhere unless the caller sets a
// writer with SetTraceWriter; there is no sampling, no parent/child
// span linking beyond shared trace ID, and no clock-skew handling.

type spanContextKey struct{}

var (
	traceMu     sync.Mutex
	traceWriter io.Writer = io.Discard
)

// SetTraceWriter sets the destination for finished-span JSON lines.
// The default is io.Discard (spans are dropped). Passing nil restores
// io.Discard. It is safe for concurrent use. Callers should set the
// writer once at startup; changing it mid-flight only affects spans
// that end afterwards.
func SetTraceWriter(w io.Writer) {
	traceMu.Lock()
	defer traceMu.Unlock()
	if w == nil {
		traceWriter = io.Discard
		return
	}
	traceWriter = w
}

func getTraceWriter() io.Writer {
	traceMu.Lock()
	defer traceMu.Unlock()
	if traceWriter == nil {
		return io.Discard
	}
	return traceWriter
}

// Span is one lightweight local trace span.
type Span struct {
	// TraceID is a 16-character lowercase hex ID (8 random bytes),
	// shared by all spans in one trace.
	TraceID string
	// SpanID is an 8-character lowercase hex ID (4 random bytes),
	// unique per span.
	SpanID string
	// Name is the span name passed to StartSpan.
	Name string

	mu     sync.Mutex
	start  time.Time
	attrs  map[string]string
	ended  bool
	writer io.Writer
}

// randomHex returns n random bytes encoded as 2n lowercase hex characters.
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively impossible; fall back to
		// time-based bytes rather than returning an empty ID.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (uint(i%8) * 8))
		}
	}
	return hex.EncodeToString(b)
}

// StartSpan starts a span named name and returns a context carrying it
// plus the span itself. If ctx already carries a span, the new span
// reuses that span's TraceID (same trace); otherwise a new TraceID is
// generated. The caller must call the returned span's End exactly once
// (End is idempotent, so a second call is a harmless no-op).
func StartSpan(ctx context.Context, name string) (context.Context, *Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	traceID := ""
	if parent, ok := ctx.Value(spanContextKey{}).(*Span); ok && parent != nil && parent.TraceID != "" {
		traceID = parent.TraceID
	}
	if traceID == "" {
		traceID = randomHex(8) // 16 hex chars
	}
	s := &Span{
		TraceID: traceID,
		SpanID:  randomHex(4), // 8 hex chars
		Name:    name,
		start:   time.Now(),
		attrs:   make(map[string]string),
		writer:  getTraceWriter(),
	}
	return context.WithValue(ctx, spanContextKey{}, s), s
}

// SpanFromContext returns the span carried by ctx, if any.
func SpanFromContext(ctx context.Context) (*Span, bool) {
	if ctx == nil {
		return nil, false
	}
	s, ok := ctx.Value(spanContextKey{}).(*Span)
	return s, ok && s != nil
}

// SetAttr records a string attribute on the span. It is safe for
// concurrent use and is a no-op after End. Do not pass secrets or
// message content as attribute values.
func (s *Span) SetAttr(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ended {
		return
	}
	if s.attrs == nil {
		s.attrs = make(map[string]string)
	}
	s.attrs[key] = value
}

// End finishes the span and writes one JSON line to the trace writer
// with fields trace_id, span_id, name, start_ms, duration_ms and attrs.
// It is idempotent: only the first call writes. It never panics on a
// nil receiver.
func (s *Span) End() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended = true
	end := time.Now()
	startMs := s.start.UnixMilli()
	durationMs := float64(end.Sub(s.start).Nanoseconds()) / 1e6
	attrs := make(map[string]string, len(s.attrs))
	for k, v := range s.attrs {
		attrs[k] = v
	}
	w := s.writer
	if w == nil {
		w = io.Discard
	}
	s.mu.Unlock()

	line := struct {
		TraceID    string            `json:"trace_id"`
		SpanID     string            `json:"span_id"`
		Name       string            `json:"name"`
		StartMs    int64             `json:"start_ms"`
		DurationMs float64           `json:"duration_ms"`
		Attrs      map[string]string `json:"attrs"`
	}{
		TraceID:    s.TraceID,
		SpanID:     s.SpanID,
		Name:       s.Name,
		StartMs:    startMs,
		DurationMs: durationMs,
		Attrs:      attrs,
	}
	enc, err := json.Marshal(line)
	if err != nil {
		return
	}
	// One line per span; ignore write errors — tracing must never
	// break the request path.
	_, _ = w.Write(append(enc, '\n'))
}
