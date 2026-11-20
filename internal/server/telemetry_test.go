// M2 telemetry wiring tests (W2): the observability Collector +
// structured request Logger fed from the A8 choke points, and the
// combined /metrics exposition. Written FIRST — these FAIL until
// telemetry.go + the call-site wiring land.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/observability"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// newTelemetryServer builds a header-priced server (prov-b/model-b,
// usage 100 in / 50 out / 10 cached, cost 0.000195 USD) with an
// injected collector and a captured JSON log writer, so the tests
// can assert exactly what the request path recorded.
func newTelemetryServer(t *testing.T) (*Server, *observability.Collector, *bytes.Buffer) {
	t.Helper()
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{
			chatResp: provider.ChatResponse{
				Content: "Hello there", Model: "model-b",
				Usage: provider.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CachedTokens: 10},
			},
			deltas: []string{"Hello", " world"},
		},
		trace: headerTrace(),
		routing: router.RoutingInfo{
			Provider: "prov-b", Model: "model-b",
			Strategy: "priority", Reason: "first-choice",
		},
		hasRouting: true,
	}
	col := observability.NewCollector()
	var logBuf bytes.Buffer
	srv := newTestServer(t, eng,
		WithVersion("test-1.2.3"),
		WithTelemetryCollector(col),
		WithTelemetryLogger(observability.NewLogger(&logBuf)),
	)
	srv.SetLedgerPrices(headerPrices())
	return srv, col, &logBuf
}

func TestTelemetryNonStreamChatRecordsObservation(t *testing.T) {
	srv, col, logBuf := newTelemetryServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"zebra-prompt-98765"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	snap := col.Snapshot()
	if snap.Requests != 1 {
		t.Fatalf("collector requests = %d, want exactly 1 (one observation per finished request)", snap.Requests)
	}
	if snap.OK != 1 || snap.Errors != 0 {
		t.Errorf("collector ok/errors = %d/%d, want 1/0", snap.OK, snap.Errors)
	}
	if snap.TokensIn != 100 || snap.TokensOut != 50 || snap.TokensCached != 10 {
		t.Errorf("collector tokens = in %d out %d cached %d, want 100/50/10", snap.TokensIn, snap.TokensOut, snap.TokensCached)
	}
	if math.Abs(snap.CostUSD-0.000195) > 1e-12 {
		t.Errorf("collector cost = %v, want 0.000195", snap.CostUSD)
	}
	if snap.LatencySamples != 1 {
		t.Errorf("collector latency samples = %d, want 1", snap.LatencySamples)
	}
	if snap.FallbackAttempts != 1 {
		t.Errorf("collector fallback attempts = %d, want 1", snap.FallbackAttempts)
	}

	// One JSON log line, with the correlation id, and no
	// prompt/response content anywhere in it.
	line := strings.TrimSpace(logBuf.String())
	if line == "" {
		t.Fatal("no request log line emitted")
	}
	if strings.Count(logBuf.String(), "\n") != 1 {
		t.Errorf("log has %d lines, want exactly 1 per finished request", strings.Count(logBuf.String(), "\n"))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		t.Fatalf("log line is not JSON: %q: %v", line, err)
	}
	if entry["request_id"] == nil || entry["request_id"] == "" {
		t.Errorf("log line request_id = %v, want non-empty", entry["request_id"])
	}
	if got := rec.Header().Get("X-Codvian-Request-Id"); entry["request_id"] != got {
		t.Errorf("log request_id = %v, want the response header id %q", entry["request_id"], got)
	}
	if entry["provider"] != "prov-b" || entry["model"] != "model-b" {
		t.Errorf("log provider/model = %v/%v, want prov-b/model-b", entry["provider"], entry["model"])
	}
	if entry["outcome"] != "ok" {
		t.Errorf("log outcome = %v, want ok", entry["outcome"])
	}
	for _, secret := range []string{"zebra-prompt-98765", "Hello there"} {
		if strings.Contains(line, secret) {
			t.Errorf("log line contains prompt/response text %q — must never be logged", secret)
		}
	}
}

func TestTelemetryStreamChatRecordsObservation(t *testing.T) {
	srv, col, _ := newTelemetryServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("body missing [DONE]:\n%s", rec.Body.String())
	}
	// The trailer is untouched (A8 rule): recording is
	// server-side only and must not alter the response body.
	if !strings.Contains(rec.Body.String(), ": codvian-meta ") {
		t.Fatalf("body missing stream trailer:\n%s", rec.Body.String())
	}
	snap := col.Snapshot()
	if snap.Requests != 1 {
		t.Fatalf("collector requests = %d, want exactly 1 for a streamed request", snap.Requests)
	}
	if snap.OK != 1 {
		t.Errorf("collector ok = %d, want 1", snap.OK)
	}
	if snap.TokensIn != 100 || snap.TokensOut != 50 {
		t.Errorf("collector tokens = in %d out %d, want 100/50", snap.TokensIn, snap.TokensOut)
	}
	if math.Abs(snap.CostUSD-0.000195) > 1e-12 {
		t.Errorf("collector cost = %v, want 0.000195", snap.CostUSD)
	}
}

func TestTelemetryEngineErrorRecordsErrorClass(t *testing.T) {
	eng := &headerTestEngine{
		fakeEngine: &fakeEngine{chatErr: errors.New("boom")},
		trace:      headerTrace(),
	}
	col := observability.NewCollector()
	srv := newTestServer(t, eng, WithVersion("test-1.2.3"), WithTelemetryCollector(col))
	srv.SetLedgerPrices(headerPrices())
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	snap := col.Snapshot()
	if snap.Requests != 1 || snap.Errors != 1 || snap.OK != 0 {
		t.Errorf("collector requests/ok/errors = %d/%d/%d, want 1/0/1", snap.Requests, snap.OK, snap.Errors)
	}
	if snap.ErrorsByClass["upstream_5xx"] != 1 {
		t.Errorf("collector errors_by_class = %v, want upstream_5xx: 1", snap.ErrorsByClass)
	}
}

func TestTelemetryCacheHitRecordsHitRatio(t *testing.T) {
	eng := &cacheHitEngine{
		fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "Paris.", Model: "model-a"}},
		kind:       "semantic",
		sim:        0.91,
	}
	col := observability.NewCollector()
	srv := newTestServer(t, eng, WithTelemetryCollector(col))
	srv.SetLedgerPrices(headerPrices())
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"capital of France?"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	snap := col.Snapshot()
	if snap.Requests != 1 || snap.OK != 1 {
		t.Errorf("collector requests/ok = %d/%d, want 1/1", snap.Requests, snap.OK)
	}
	if snap.CacheHits != 1 || snap.CacheMisses != 0 {
		t.Errorf("collector cache hits/misses = %d/%d, want 1/0", snap.CacheHits, snap.CacheMisses)
	}
	if snap.CacheHitRatio != 1 {
		t.Errorf("collector cache hit ratio = %v, want 1", snap.CacheHitRatio)
	}
	if snap.CacheHitsByLayer["semantic"] != 1 {
		t.Errorf("collector cache hits by layer = %v, want semantic: 1", snap.CacheHitsByLayer)
	}
	// A cache hit bills nobody: the known $0, never a guess.
	if snap.CostUSD != 0 {
		t.Errorf("collector cost = %v, want 0 for a cache hit", snap.CostUSD)
	}
}

func TestTelemetryMetricsExposesCollector(t *testing.T) {
	srv, col, _ := newTelemetryServer(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if col.Snapshot().Requests != 1 {
		t.Fatal("setup: expected 1 recorded request before scraping /metrics")
	}
	mrec := doReq(t, srv, http.MethodGet, "/metrics", "", "test-key-123")
	if mrec.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200 (collector present, recorder nil)", mrec.Code)
	}
	body := mrec.Body.String()
	for _, want := range []string{
		`codvian_router_requests_total{provider="prov-b",model="model-b",outcome="ok"} 1`,
		`codvian_router_tokens_total{type="in"} 100`,
		`codvian_router_cost_usd_total 0.000195`,
		`codvian_router_fallback_attempts_total{provider="prov-b",model="model-b"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestTelemetryMetricsBothPresentAndBothAbsent(t *testing.T) {
	// Both recorder and collector: 200, both expositions present.
	srv, col, _ := newTelemetryServer(t)
	srv2 := newTestServer(t, &fakeEngine{}, WithMetrics(metrics.NewRecorder()), WithTelemetryCollector(col))
	_ = srv
	mrec := doReq(t, srv2, http.MethodGet, "/metrics", "", "test-key-123")
	if mrec.Code != http.StatusOK {
		t.Fatalf("/metrics (both) status = %d, want 200", mrec.Code)
	}
	body := mrec.Body.String()
	if !strings.Contains(body, "codvian_router_upstream_failures_total") {
		t.Errorf("/metrics (both) missing recorder-only series:\n%s", body)
	}
	if !strings.Contains(body, "codvian_router_cost_usd_total") {
		t.Errorf("/metrics (both) missing collector-only series:\n%s", body)
	}

	// Neither recorder nor collector: 503.
	srv3, _ := newHeaderServer(t)
	srv3.obsCollector = nil
	srv3.obsLogger = nil
	mrec = doReq(t, srv3, http.MethodGet, "/metrics", "", "test-key-123")
	if mrec.Code != http.StatusServiceUnavailable {
		t.Errorf("/metrics (neither) status = %d, want 503", mrec.Code)
	}
}

func TestTelemetryNilCollectorLoggerNeverPanics(t *testing.T) {
	srv, _ := newHeaderServer(t)
	srv.obsCollector = nil
	srv.obsLogger = nil
	// Direct call with nil fields must not panic.
	srv.recordTelemetry(context.Background(), ResponseMeta{Provider: "prov-b", Model: "model-b"}, http.StatusOK)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with telemetry disabled, body = %s", rec.Code, rec.Body.String())
	}
	// Stream path with telemetry off must not panic either.
	rec = doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"model-b","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("stream status = %d, want 200 with telemetry disabled, body = %s", rec.Code, rec.Body.String())
	}
}
