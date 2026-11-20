package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func body(t *testing.T, r *Recorder) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Handler status = %d, want 200", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain-ish", ct)
	}
	return rec.Body.String()
}

func TestHandlerStatusAndContentType(t *testing.T) {
	r := NewRecorder()
	_ = body(t, r)
}

func TestCountersIncrementAndRender(t *testing.T) {
	r := NewRecorder()
	r.IncRequest("openai", "gpt-4o", "ok")
	r.IncRequest("openai", "gpt-4o", "ok")
	r.IncRequest("openai", "gpt-4o", "error")
	r.IncRequest("anthropic", "claude-3", "rate_limited")
	r.IncFailover("openai", "anthropic")
	r.IncFailover("openai", "anthropic")
	r.IncCacheHit("exact")
	r.IncCacheHit("semantic")
	r.IncCacheMiss("exact")
	r.IncBudgetRefused()
	r.IncBudgetRefused()
	r.IncBudgetRefused()
	r.SetInFlight(3)

	out := body(t, r)
	want := []string{
		`codvian_router_requests_total{provider="openai",model="gpt_4o",outcome="ok"} 2`,
		`codvian_router_requests_total{provider="openai",model="gpt_4o",outcome="error"} 1`,
		`codvian_router_requests_total{provider="anthropic",model="claude_3",outcome="rate_limited"} 1`,
		`codvian_router_failovers_total{from_provider="openai",to_provider="anthropic"} 2`,
		`codvian_router_cache_hits_total{layer="exact"} 1`,
		`codvian_router_cache_hits_total{layer="semantic"} 1`,
		`codvian_router_cache_misses_total{layer="exact"} 1`,
		`codvian_router_budget_refused_total 3`,
		`codvian_router_in_flight 3`,
		`# TYPE codvian_router_requests_total counter`,
		`# TYPE codvian_router_in_flight gauge`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\nfull output:\n%s", w, out)
		}
	}
}

func TestInFlightIncDecClampsAtZero(t *testing.T) {
	r := NewRecorder()
	r.DecInFlight() // no underflow
	r.IncInFlight()
	r.IncInFlight()
	r.DecInFlight()
	if !strings.Contains(body(t, r), "codvian_router_in_flight 1") {
		t.Errorf("expected in_flight 1, got:\n%s", body(t, r))
	}
	r.SetInFlight(-5)
	if !strings.Contains(body(t, r), "codvian_router_in_flight 0") {
		t.Errorf("expected in_flight clamped to 0, got:\n%s", body(t, r))
	}
}

func TestHistogramBucketsSumCount(t *testing.T) {
	r := NewRecorder()
	// Latencies: 1, 5, 12, 100, 10000, 20000 (> largest bucket).
	for _, ms := range []float64{1, 5, 12, 100, 10000, 20000} {
		r.ObserveLatency("openai", "gpt-4o", ms)
	}
	out := body(t, r)
	// Cumulative buckets: le=1:1, le=5:2, le=10:2, le=25:3, le=50:3,
	// le=100:4, le=250:4, le=500:4, le=1000:4, le=2500:4, le=5000:4,
	// le=10000:5, le=+Inf:6. Sum = 30118, count = 6.
	want := map[string]string{
		`le="1"`:     "1",
		`le="5"`:     "2",
		`le="10"`:    "2",
		`le="25"`:    "3",
		`le="50"`:    "3",
		`le="100"`:   "4",
		`le="250"`:   "4",
		`le="500"`:   "4",
		`le="1000"`:  "4",
		`le="2500"`:  "4",
		`le="5000"`:  "4",
		`le="10000"`: "5",
		`le="+Inf"`:  "6",
	}
	for le, count := range want {
		line := `codvian_router_request_latency_ms_bucket{provider="openai",model="gpt_4o",` + le + `} ` + count
		if !strings.Contains(out, line) {
			t.Errorf("missing histogram line %q\nfull output:\n%s", line, out)
		}
	}
	if !strings.Contains(out, `codvian_router_request_latency_ms_sum{provider="openai",model="gpt_4o"} 30118`) {
		t.Errorf("missing/wrong _sum line\nfull output:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_request_latency_ms_count{provider="openai",model="gpt_4o"} 6`) {
		t.Errorf("missing/wrong _count line\nfull output:\n%s", out)
	}
}

func TestLabelSanitization(t *testing.T) {
	if got := sanitizeLabel("gpt-4o mini!v2"); got != "gpt_4o_mini_v2" {
		t.Errorf("sanitizeLabel = %q, want %q", got, "gpt_4o_mini_v2")
	}
	if got := sanitizeLabel("a:b_c-1.2"); got != "a:b_c_1_2" {
		t.Errorf("sanitizeLabel = %q, want %q", got, "a:b_c_1_2")
	}
	// Allowed chars survive.
	if got := sanitizeLabel("openai:chat_v1"); got != "openai:chat_v1" {
		t.Errorf("sanitizeLabel = %q, want unchanged", got)
	}

	r := NewRecorder()
	r.IncRequest("", "", "ok")
	r.IncRequest("bad provider!", "model/x", "ok")
	r.ObserveLatency("", "weird model!", 7)
	out := body(t, r)
	if !strings.Contains(out, `codvian_router_requests_total{provider="unknown",model="unknown",outcome="ok"} 1`) {
		t.Errorf("empty provider/model not mapped to unknown:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_requests_total{provider="bad_provider_",model="model_x",outcome="ok"} 1`) {
		t.Errorf("unsanitized labels leaked into exposition:\n%s", out)
	}
	if !strings.Contains(out, `codvian_router_request_latency_ms_count{provider="unknown",model="weird_model_"} 1`) {
		t.Errorf("histogram empty/weird labels wrong:\n%s", out)
	}
}

func TestConcurrentIncObserve(t *testing.T) {
	r := NewRecorder()
	var wg sync.WaitGroup
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				r.IncRequest("openai", "gpt-4o", "ok")
				r.ObserveLatency("openai", "gpt-4o", float64(i%100))
				r.IncFailover("openai", "anthropic")
				r.IncCacheHit("exact")
				r.IncCacheMiss("semantic")
				r.IncBudgetRefused()
				r.IncInFlight()
				r.DecInFlight()
			}
		}(g)
	}
	wg.Wait()
	out := body(t, r)
	// 10 goroutines x 200 = 2000 each.
	for _, want := range []string{
		`codvian_router_requests_total{provider="openai",model="gpt_4o",outcome="ok"} 2000`,
		`codvian_router_failovers_total{from_provider="openai",to_provider="anthropic"} 2000`,
		`codvian_router_cache_hits_total{layer="exact"} 2000`,
		`codvian_router_cache_misses_total{layer="semantic"} 2000`,
		`codvian_router_budget_refused_total 2000`,
		`codvian_router_request_latency_ms_count{provider="openai",model="gpt_4o"} 2000`,
		`codvian_router_in_flight 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestSpanJSONLine(t *testing.T) {
	var buf bytes.Buffer
	SetTraceWriter(&buf)
	defer SetTraceWriter(nil)

	ctx, span := StartSpan(context.Background(), "router.chat")
	if len(span.TraceID) != 16 {
		t.Errorf("TraceID = %q (%d chars), want 16 hex chars", span.TraceID, len(span.TraceID))
	}
	if len(span.SpanID) != 8 {
		t.Errorf("SpanID = %q (%d chars), want 8 hex chars", span.SpanID, len(span.SpanID))
	}
	span.SetAttr("provider", "openai")
	span.SetAttr("outcome", "ok")
	time.Sleep(2 * time.Millisecond)
	span.End()
	span.End() // idempotent: must not write a second line

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 trace line, got %d: %q", len(lines), buf.String())
	}
	var got struct {
		TraceID    string            `json:"trace_id"`
		SpanID     string            `json:"span_id"`
		Name       string            `json:"name"`
		StartMs    int64             `json:"start_ms"`
		DurationMs float64           `json:"duration_ms"`
		Attrs      map[string]string `json:"attrs"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("trace line is not valid JSON: %v (%q)", err, lines[0])
	}
	if got.TraceID != span.TraceID || got.SpanID != span.SpanID {
		t.Errorf("ids mismatch: got trace %q span %q, want %q / %q", got.TraceID, got.SpanID, span.TraceID, span.SpanID)
	}
	if got.Name != "router.chat" {
		t.Errorf("name = %q, want router.chat", got.Name)
	}
	if got.StartMs <= 0 {
		t.Errorf("start_ms = %d, want > 0", got.StartMs)
	}
	if got.DurationMs < 0 {
		t.Errorf("duration_ms = %v, want >= 0", got.DurationMs)
	}
	if got.Attrs["provider"] != "openai" || got.Attrs["outcome"] != "ok" {
		t.Errorf("attrs = %v, want provider=openai outcome=ok", got.Attrs)
	}
	// Child span shares the parent trace ID.
	_, child := StartSpan(ctx, "child")
	if child.TraceID != span.TraceID {
		t.Errorf("child TraceID = %q, want parent %q", child.TraceID, span.TraceID)
	}
	if child.SpanID == span.SpanID {
		t.Errorf("child SpanID equals parent SpanID %q", child.SpanID)
	}
	child.End()
}

func TestSpanDefaultWriterDiscards(t *testing.T) {
	SetTraceWriter(nil)
	_, span := StartSpan(context.Background(), "noop")
	span.End() // must not panic with the default Discard writer
	var nilSpan *Span
	nilSpan.End() // must not panic on nil receiver
}

func TestIncCoalescedShared(t *testing.T) {
	r := NewRecorder()
	r.IncCoalescedShared()
	r.IncCoalescedShared()
	r.IncCoalescedShared()
	if got := r.Coalesced(); got != 3 {
		t.Fatalf("Coalesced() = %d, want 3", got)
	}
	out := body(t, r)
	if !strings.Contains(out, "codvian_router_coalesced_total 3\n") {
		t.Fatalf("exposition missing coalesced counter 3:\n%s", out)
	}
}

func TestConcurrencyLimitAndRejected(t *testing.T) {
	r := NewRecorder()
	r.SetConcurrencyLimit("prov-slow", 2.5)
	r.SetConcurrencyLimit("prov-fast", 20)
	r.IncConcurrencyRejected("prov-slow")
	r.IncConcurrencyRejected("prov-slow")
	out := body(t, r)
	if !strings.Contains(out, "codvian_router_concurrency_limit{provider=\"prov_slow\"} 2.5\n") {
		t.Fatalf("exposition missing slow limit 2.5:\n%s", out)
	}
	if !strings.Contains(out, "codvian_router_concurrency_rejected_total{provider=\"prov_slow\"} 2\n") {
		t.Fatalf("exposition missing slow rejected 2:\n%s", out)
	}
}

// TestPanicsCounter proves the recovered-panic counter (CHECKLIST
// 165): per-location increments, an honest zero before any panic
// (no series rendered), the total in Snapshot, and sanitized
// location labels.
func TestPanicsCounter(t *testing.T) {
	r := NewRecorder()
	if got := r.Panics(); got != 0 {
		t.Fatalf("Panics() = %d, want 0 before any panic", got)
	}
	if got := r.Snapshot().Panics; got != 0 {
		t.Fatalf("Snapshot().Panics = %d, want 0 before any panic", got)
	}
	if out := body(t, r); strings.Contains(out, "codvian_router_panics_total{") {
		t.Fatalf("metrics render a panics series before any panic:\n%s", out)
	}

	r.IncPanic("provider_chat")
	r.IncPanic("provider_stream")
	r.IncPanic("provider_stream")
	r.IncPanic("weird location!") // sanitized to weird_location_
	if got := r.Panics(); got != 4 {
		t.Fatalf("Panics() = %d, want 4", got)
	}
	if got := r.Snapshot().Panics; got != 4 {
		t.Fatalf("Snapshot().Panics = %d, want 4", got)
	}
	out := body(t, r)
	for _, want := range []string{
		`codvian_router_panics_total{location="provider_chat"} 1`,
		`codvian_router_panics_total{location="provider_stream"} 2`,
		`codvian_router_panics_total{location="weird_location_"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metrics missing %q:\n%s", want, out)
		}
	}
}
