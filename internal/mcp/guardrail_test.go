package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// TestToolCallGuardrailRefusal proves the MCP tool_args phase
// (CHECKLIST 147): a tools/call whose arguments trip a blocking
// guardrail is refused with the Codvian policy-refusal code -32001
// before dispatch — never -32602 (invalid params) and never -32603
// (an upstream failure, which cannot happen on read-only tools).
// The refusal names the kind only; the argument content is never
// echoed back. With blocking off the same call flows, and the
// evaluation is still counted under phase tool_args.
func TestToolCallGuardrailRefusal(t *testing.T) {
	key := "sk-proj-abcdefghij1234567890"
	secretArgs := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"router_status","arguments":{"note":"my key is ` + key + `"}}}`
	cleanArgs := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"router_status","arguments":{"note":"hello"}}}`

	// No guardrail wired: the old behaviour, untouched.
	out := call(t, newTestServer(), secretArgs)
	if _, ok := out["result"]; !ok {
		t.Fatalf("no-guard server refused a secret-args call: %v", out)
	}

	// Guardrail wired, blocking off: the call flows, evaluation counted.
	store := guardrails.NewStore()
	srv := newTestServer().WithGuardrail(store)
	out = call(t, srv, secretArgs)
	if _, ok := out["result"]; !ok {
		t.Fatalf("blocking-off server refused a secret-args call: %v", out)
	}
	if got := store.Stats().Evaluations; len(got) != 1 || got[0].Phase != guardrails.PhaseMCPToolArgs || got[0].Action != guardrails.ActionRedacted {
		t.Fatalf("store evaluations = %+v, want one tool_args/redacted", got)
	}

	// Guardrail wired, BlockSecrets on: the call is refused with -32001.
	blocking := guardrails.NewStoreWith(guardrails.Config{BlockSecrets: true})
	srv = newTestServer().WithGuardrail(blocking)
	raw := srv.Handle(context.Background(), []byte(secretArgs))
	if raw == nil {
		t.Fatal("blocked tools/call returned no response")
	}
	var errResp struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &errResp); err != nil {
		t.Fatalf("refusal is not JSON: %v (%s)", err, raw)
	}
	if errResp.Error.Code != guardrails.PolicyRefusalCode {
		t.Fatalf("refusal code = %d, want %d (%s)", errResp.Error.Code, guardrails.PolicyRefusalCode, raw)
	}
	if !strings.Contains(errResp.Error.Message, guardrails.KindSecret) {
		t.Fatalf("refusal message = %q, want it to name the kind", errResp.Error.Message)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("refusal echoes the secret argument back")
	}
	if got := blocking.Stats().Evaluations; len(got) != 1 || got[0].Phase != guardrails.PhaseMCPToolArgs || got[0].Action != guardrails.ActionBlocked {
		t.Fatalf("store evaluations = %+v, want one tool_args/blocked", got)
	}

	// The same server still serves a clean call, and an unknown
	// tool still answers -32602 — the refusal code is reserved
	// for policy refusals, not a catch-all.
	out = call(t, srv, cleanArgs)
	if _, ok := out["result"]; !ok {
		t.Fatalf("blocking server refused a clean call: %v", out)
	}
	out = call(t, srv, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"no_such_tool","arguments":{}}}`)
	errObj, _ := out["error"].(map[string]any)
	if errObj == nil || errObj["code"] != float64(-32602) {
		t.Fatalf("unknown tool error = %v, want code -32602", out["error"])
	}
}
