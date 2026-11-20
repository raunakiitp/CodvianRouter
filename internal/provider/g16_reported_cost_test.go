package provider

import (
	"encoding/json"
	"testing"
)

// G16 Area 11 gateway-reported cost: the OpenAI-wire usage block
// decodes the two reported-cost shapes into Usage.Cost.
func TestG16UsageWireReportedCost(t *testing.T) {
	// OpenRouter: usage.cost, bare USD.
	var openRouter chatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"x","model":"m","choices":[],
		"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost":0.0042}}`), &openRouter); err != nil {
		t.Fatalf("unmarshal openrouter: %v", err)
	}
	u := openRouter.usage()
	if u.Cost == nil || *u.Cost != 0.0042 {
		t.Fatalf("openrouter usage cost = %v, want 0.0042", u.Cost)
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 {
		t.Fatalf("openrouter tokens = %d/%d, want 100/50", u.PromptTokens, u.CompletionTokens)
	}

	// xAI: cost_in_usd_ticks, 1e10 ticks per USD. 37,756,000 ticks
	// ≈ $0.0038 (the xAI docs example).
	var xai chatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"x","model":"m","choices":[],
		"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost_in_usd_ticks":37756000}}`), &xai); err != nil {
		t.Fatalf("unmarshal xai: %v", err)
	}
	xu := xai.usage()
	if xu.Cost == nil || *xu.Cost != 37756000.0/1e10 {
		t.Fatalf("xai usage cost = %v, want %v", xu.Cost, 37756000.0/1e10)
	}

	// Both present: ticks win (the more specific wire shape).
	var both chatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"x","model":"m","choices":[],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":9.99,"cost_in_usd_ticks":10000000000}}`), &both); err != nil {
		t.Fatalf("unmarshal both: %v", err)
	}
	bu := both.usage()
	if bu.Cost == nil || *bu.Cost != 1.0 {
		t.Fatalf("both-present usage cost = %v, want 1.0 (ticks win)", bu.Cost)
	}

	// Negative / absent: no cost is reported, never a nonsense one.
	var neg chatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"x","model":"m","choices":[],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"cost":-5}}`), &neg); err != nil {
		t.Fatalf("unmarshal negative: %v", err)
	}
	if neg.usage().Cost != nil {
		t.Fatalf("negative reported cost accepted: %v", neg.usage().Cost)
	}
	var plain chatCompletionResponse
	if err := json.Unmarshal([]byte(`{"id":"x","model":"m","choices":[],
		"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`), &plain); err != nil {
		t.Fatalf("unmarshal plain: %v", err)
	}
	if plain.usage().Cost != nil {
		t.Fatalf("plain usage gained a cost: %v", plain.usage().Cost)
	}
}

// G16 Area 11 batch/flex: service_tier decodes from the client
// request into ChatRequest and round-trips.
func TestG16ChatRequestServiceTier(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"service_tier":"flex"}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ServiceTier != "flex" {
		t.Fatalf("ServiceTier = %q, want flex", req.ServiceTier)
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var req2 ChatRequest
	if err := json.Unmarshal(out, &req2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if req2.ServiceTier != "flex" {
		t.Fatalf("round-tripped ServiceTier = %q, want flex", req2.ServiceTier)
	}
	// Absent tier stays "" (standard), and the field is omitted
	// on the wire when unset.
	var req3 ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[]}`), &req3); err != nil {
		t.Fatalf("unmarshal no-tier: %v", err)
	}
	if req3.ServiceTier != "" {
		t.Fatalf("ServiceTier = %q, want empty", req3.ServiceTier)
	}
}
