package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/observability"
)

// G10 Area 12 row 9: GET /api/telemetry/model-latency-stats handler
// tests. The handler is invoked directly here (srv.
// handleModelLatencyStats) — route registration (guard-table row +
// s.register call) lands with the coordinator at the next gate, the
// same pattern as the cache-stats handler tests. Every percentile
// asserted below is read back from the same live Collector the
// handler reads, seeded with known samples: 1..100 ms for
// prov-a/m1 (p50 50, p95 95, p99 99, nearest-rank) and 10 samples
// at 10..100 ms for prov-b/m2 (p50 50, p95 100, p99 100).
func latencyStatsStack(t *testing.T) (*Server, *observability.Collector) {
	t.Helper()
	c := observability.NewCollector()
	for i := 1; i <= 100; i++ {
		c.Record(observability.Observation{Provider: "prov-a", Model: "m1", LatencyMs: float64(i), Outcome: "ok"})
	}
	for i := 1; i <= 10; i++ {
		c.Record(observability.Observation{Provider: "prov-b", Model: "m2", LatencyMs: float64(i * 10), Outcome: "ok"})
	}
	srv := newTestServer(t, &fakeEngine{}, WithTelemetryCollector(c))
	return srv, c
}

func callModelLatencyStats(t *testing.T, srv *Server, path string) (int, modelLatencyStatsResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	w := httptest.NewRecorder()
	srv.handleModelLatencyStats(w, req)
	var out modelLatencyStatsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode model latency stats: %v (body %s)", err, w.Body.String())
	}
	return w.Code, out
}

func TestModelLatencyStatsHandler(t *testing.T) {
	srv, _ := latencyStatsStack(t)
	code, out := callModelLatencyStats(t, srv, "/api/telemetry/model-latency-stats")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if out.Count != 2 || len(out.Stats) != 2 {
		t.Fatalf("count/stats = %d/%d, want 2/2", out.Count, len(out.Stats))
	}
	a := out.Stats[0]
	if a.Provider != "prov-a" || a.Model != "m1" || a.Samples != 100 {
		t.Fatalf("first row = %+v, want prov-a/m1 with 100 samples", a)
	}
	if a.P50Ms != 50 || a.P95Ms != 95 || a.P99Ms != 99 {
		t.Errorf("prov-a/m1 percentiles = %v/%v/%v, want 50/95/99", a.P50Ms, a.P95Ms, a.P99Ms)
	}
	b := out.Stats[1]
	if b.Provider != "prov-b" || b.Model != "m2" || b.Samples != 10 {
		t.Fatalf("second row = %+v, want prov-b/m2 with 10 samples", b)
	}
	if b.P50Ms != 50 || b.P95Ms != 100 || b.P99Ms != 100 {
		t.Errorf("prov-b/m2 percentiles = %v/%v/%v, want 50/100/100", b.P50Ms, b.P95Ms, b.P99Ms)
	}
}

func TestModelLatencyStatsFilters(t *testing.T) {
	srv, _ := latencyStatsStack(t)
	_, out := callModelLatencyStats(t, srv, "/api/telemetry/model-latency-stats?provider=prov-b")
	if out.Count != 1 || len(out.Stats) != 1 || out.Stats[0].Provider != "prov-b" {
		t.Errorf("provider filter = %+v, want exactly the prov-b row", out)
	}
	_, out = callModelLatencyStats(t, srv, "/api/telemetry/model-latency-stats?model=m1")
	if out.Count != 1 || len(out.Stats) != 1 || out.Stats[0].Model != "m1" {
		t.Errorf("model filter = %+v, want exactly the m1 row", out)
	}
	_, out = callModelLatencyStats(t, srv, "/api/telemetry/model-latency-stats?provider=prov-a&model=m2")
	if out.Count != 0 || len(out.Stats) != 0 {
		t.Errorf("non-matching filter = %+v, want empty list (not null)", out)
	}
	if out.Stats == nil {
		t.Error("Stats is nil on empty result, want an empty list []")
	}
}

func TestModelLatencyStatsCollectorNilIs503(t *testing.T) {
	// WithTelemetryCollector(nil) disables collection (telemetry.go):
	// the handler must answer 503, never an empty list that would
	// read as "no latency data yet".
	srv := newTestServer(t, &fakeEngine{}, WithTelemetryCollector(nil))
	req := httptest.NewRequest(http.MethodGet, "/api/telemetry/model-latency-stats", nil)
	w := httptest.NewRecorder()
	srv.handleModelLatencyStats(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status with nil collector = %d, want 503 (telemetry off, never empty data)", w.Code)
	}
}

func TestModelLatencyStatsMethodNotAllowed(t *testing.T) {
	srv, _ := latencyStatsStack(t)
	req := httptest.NewRequest(http.MethodPost, "/api/telemetry/model-latency-stats", nil)
	w := httptest.NewRecorder()
	srv.handleModelLatencyStats(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", w.Code)
	}
}
