package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// reasoningEngine is a fakeEngine that also reports reasoning
// replay counters and a fixed last trace, for the dashboard feeds.
type reasoningEngine struct {
	*fakeEngine
	stats reasoning.Stats
	trace *routetrace.RouteTrace
}

func (e *reasoningEngine) ReasoningStats() reasoning.Stats { return e.stats }
func (e *reasoningEngine) LastTrace() *routetrace.RouteTrace {
	return e.trace
}

// TestChatCompletionsReasoningUnrepresentableIs400 proves the
// honest failure at the HTTP layer: when no target can represent
// the restored reasoning state, the request answers 400 naming
// the wires — never a 502, and never counted as an upstream
// failure.
func TestChatCompletionsReasoningUnrepresentableIs400(t *testing.T) {
	eng := &fakeEngine{chatErr: &reasoning.UnrepresentableError{
		Wire:   reasoning.WireCohere,
		Detail: "cohere chat api v2 has no reasoning replay field",
	}}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"command-r","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reasoning state cannot be represented by the selected provider(s)") {
		t.Fatalf("body = %s, want the honest reasoning message", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cohere") {
		t.Fatalf("body = %s, want the wire named", rec.Body.String())
	}
	// The reasoning text itself must never appear in the error:
	// the detail names wires and models only.
	if strings.Contains(rec.Body.String(), "let me think") {
		t.Fatalf("body leaks reasoning content: %s", rec.Body.String())
	}
}

// TestDashboardReasoningFeed proves GET /dashboard/reasoning
// serves the live replay counters, and an honest zero state when
// the engine reports nothing.
func TestDashboardReasoningFeed(t *testing.T) {
	eng := &reasoningEngine{
		fakeEngine: &fakeEngine{},
		stats:      reasoning.Stats{Entries: 2, Captures: 3, Hits: 2, Misses: 1, Injected: 2, Evicted: 0},
	}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/reasoning", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var stats reasoning.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if stats != eng.stats {
		t.Fatalf("stats = %+v, want %+v", stats, eng.stats)
	}

	// No reporter: zero counters, never invented figures.
	srv2 := newTestServer(t, &fakeEngine{})
	rec2 := doReq(t, srv2, http.MethodGet, "/dashboard/reasoning", "", "")
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	var zero reasoning.Stats
	if err := json.Unmarshal(rec2.Body.Bytes(), &zero); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if zero != (reasoning.Stats{}) {
		t.Fatalf("stats = %+v, want zero", zero)
	}
}

// TestDashboardTraceCarriesReasoning proves GET /dashboard/trace
// carries the per-hop reasoning handoff label on each attempt.
func TestDashboardTraceCarriesReasoning(t *testing.T) {
	trace := &routetrace.RouteTrace{}
	trace.AddWithReasoning("anthropic", "claude-sonnet-4-5", 120, "rate_limited", nil, "rate_limited", "preserved: 1 thinking block(s) replayed verbatim")
	trace.AddWithReasoning("gemini", "gemini-2.5-pro", 340, "success", nil, "", "translated: thinking text → gemini thought part; anthropic signature dropped (no gemini equivalent)")
	eng := &reasoningEngine{fakeEngine: &fakeEngine{}, trace: trace}
	srv := newTestServer(t, eng)
	rec := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp traceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if len(resp.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(resp.Attempts))
	}
	if resp.Attempts[0].Reasoning != "preserved: 1 thinking block(s) replayed verbatim" {
		t.Fatalf("attempt 0 reasoning = %q", resp.Attempts[0].Reasoning)
	}
	if !strings.HasPrefix(resp.Attempts[1].Reasoning, "translated:") {
		t.Fatalf("attempt 1 reasoning = %q, want it to say translated", resp.Attempts[1].Reasoning)
	}
}
