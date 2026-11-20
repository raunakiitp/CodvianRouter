package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// TestDashboardTraceCarriesHedge proves GET /dashboard/trace
// carries the hedge decision (CHECKLIST 191): "armed" with the
// real delay for a default chain, "disabled-explicit-order" for
// an explicit-order chain, and nothing when no decision was
// recorded — so the Live Routing view can distinguish a hedged
// request from an explicit-order one honestly.
func TestDashboardTraceCarriesHedge(t *testing.T) {
	armed := &routetrace.RouteTrace{Hedge: routetrace.HedgeModeArmed, HedgeDelayMs: 150}
	armed.Add("slow", "m", 300, "hedged_cancelled", nil)
	armed.Add("fast", "m", 40, "success", nil)
	eng := &reasoningEngine{fakeEngine: &fakeEngine{}, trace: armed}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp traceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if resp.Hedge != routetrace.HedgeModeArmed {
		t.Fatalf("hedge = %q, want %q", resp.Hedge, routetrace.HedgeModeArmed)
	}
	if resp.HedgeDelayMs != 150 {
		t.Fatalf("hedge_delay_ms = %d, want 150", resp.HedgeDelayMs)
	}

	explicit := &routetrace.RouteTrace{Hedge: routetrace.HedgeModeDisabledExplicitOrder}
	explicit.Add("slow", "m", 300, "success", nil)
	eng2 := &reasoningEngine{fakeEngine: &fakeEngine{}, trace: explicit}
	srv2 := newTestServer(t, eng2)
	rec2 := doReq(t, srv2, http.MethodGet, "/dashboard/trace", "", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	var resp2 traceResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp2.Hedge != routetrace.HedgeModeDisabledExplicitOrder {
		t.Fatalf("hedge = %q, want %q", resp2.Hedge, routetrace.HedgeModeDisabledExplicitOrder)
	}
	if resp2.HedgeDelayMs != 0 {
		t.Fatalf("hedge_delay_ms = %d, want 0 (no hedge, no delay)", resp2.HedgeDelayMs)
	}

	// No trace at all: honest empty feed, no hedge field.
	srv3 := newTestServer(t, &fakeEngine{})
	rec3 := doReq(t, srv3, http.MethodGet, "/dashboard/trace", "", "")
	if rec3.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec3.Code)
	}
	var resp3 traceResponse
	if err := json.Unmarshal(rec3.Body.Bytes(), &resp3); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp3.Hedge != "" {
		t.Fatalf("hedge = %q, want empty (no request yet)", resp3.Hedge)
	}
}
