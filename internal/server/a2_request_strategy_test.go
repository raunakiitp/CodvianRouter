// Per-request "strategy" body field validation (A2-1).
//
// The server validates the "strategy" request-body field at decode:
// a known strategy name (canonical or alias) passes, an unknown one is
// a 400 naming it, never a silent fallback to priority.
package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func stratReq() *provider.ChatRequest {
	return &provider.ChatRequest{
		Model:    "m",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	}
}

// TestValidateChatRequestStrategyFieldAcceptAndReject: a known strategy
// name (and a known alias) passes validation; an unknown one is refused
// with a message that names the field. Fails if the validation block is
// removed (then the unknown name would pass).
func TestValidateChatRequestStrategyFieldAcceptAndReject(t *testing.T) {
	for _, ok := range []string{"fusion", "priority", "cheapest", "pipeline", "jev", "sla-aware", "cheap"} {
		req := stratReq()
		req.Strategy = ok
		if msg := validateChatRequest(req); msg != "" {
			t.Errorf("validateChatRequest(strategy=%q) = %q, want ok", ok, msg)
		}
	}
	for _, bad := range []string{"not-a-strategy", "fuzion"} {
		req := stratReq()
		req.Strategy = bad
		msg := validateChatRequest(req)
		if msg == "" {
			t.Fatalf("validateChatRequest(strategy=%q) = ok, want a 400", bad)
		}
		if !strings.Contains(msg, "strategy") {
			t.Errorf("validateChatRequest(strategy=%q) = %q, want it to name the strategy field", bad, msg)
		}
	}
}

// TestChatRequestDecodesStrategyField: the "strategy" body field is a
// real decoded field on the chat request (not silently dropped). Fails
// if the provider.ChatRequest.Strategy field is removed (json decode
// then leaves it empty).
func TestChatRequestDecodesStrategyField(t *testing.T) {
	var req provider.ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"strategy":"fusion"}`), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Strategy != "fusion" {
		t.Fatalf("decoded Strategy = %q, want fusion", req.Strategy)
	}
}
