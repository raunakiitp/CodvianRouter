package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// countingSource wraps fakeSource and counts how many times each
// tool's backing read ran — the proof that a denied call never
// reached dispatch.
type countingSource struct {
	*fakeSource
	statusCalls   int
	providerCalls int
	modelCalls    int
}

func (c *countingSource) Status() StatusInfo {
	c.statusCalls++
	return c.fakeSource.Status()
}
func (c *countingSource) Providers() []ProviderInfo {
	c.providerCalls++
	return c.fakeSource.Providers()
}
func (c *countingSource) Models() []ModelInfo {
	c.modelCalls++
	return c.fakeSource.Models()
}

func newCountingServer() (*Server, *countingSource) {
	src := &countingSource{fakeSource: &fakeSource{
		providers: []ProviderInfo{{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}}},
		models:    []ModelInfo{{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"}},
		status:    StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 1, EnabledProviders: 1, Models: 1, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1},
	}}
	return New(src, "0.1.0"), src
}

func callErrCode(t *testing.T, s *Server, msg string) (float64, string) {
	t.Helper()
	raw := s.Handle(context.Background(), []byte(msg))
	if raw == nil {
		t.Fatalf("Handle(%s) returned nil, want a response", msg)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	errBody, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error response, got %v", out)
	}
	msgStr, _ := errBody["message"].(string)
	return errBody["code"].(float64), msgStr
}

func TestExplicitLocalOperatorFullScope(t *testing.T) {
	// The stdio identity, opted into EXPLICITLY: a server with
	// WithClientIdentity(LocalOperator()) allows all three tools
	// — the local operator holds `*`. This is what production
	// stdio does (runMCPTo); the bare New() default is deny-all
	// (see TestNewDefaultsToAnonymousDenyAll).
	srv, src := newCountingServer()
	srv.WithClientIdentity(LocalOperator())
	for _, tool := range []string{"router_status", "provider_list", "model_list"} {
		out := call(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if _, ok := out["result"]; !ok {
			t.Errorf("explicit local-operator server denied %s: %v", tool, out)
		}
	}
	if src.statusCalls != 1 || src.providerCalls != 1 || src.modelCalls != 1 {
		t.Errorf("dispatch counts = status %d providers %d models %d, want 1 each", src.statusCalls, src.providerCalls, src.modelCalls)
	}
	// The identity itself: kind local-operator, full grant.
	id := LocalOperator()
	if id.Kind != KindLocalOperator || id.ID != "local-operator" {
		t.Errorf("LocalOperator() = %+v, want kind local-operator id local-operator", id)
	}
	if !id.HasScope(ScopeReadStatus) || !id.HasScope(ScopeDestructive) {
		t.Errorf("local operator lacks a scope, want full grant: %+v", id.Scopes)
	}
}

func TestNarrowKeyDeniedBeforeDispatch(t *testing.T) {
	srv, src := newCountingServer()
	// A gateway key holding only mcp:read:providers.
	srv.WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadProviders}))
	// router_status: denied with the scope code, distinct from
	// the guardrail code -32001 and from -32602.
	code, msg := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`)
	if code != float64(ScopeDeniedCode) {
		t.Errorf("denied call code = %v, want %d", code, ScopeDeniedCode)
	}
	if !strings.Contains(msg, ScopeReadStatus) {
		t.Errorf("denial message %q does not name the missing scope", msg)
	}
	// PROOF the handler never ran: the Status read count is 0.
	if src.statusCalls != 0 {
		t.Errorf("denied router_status reached dispatch: statusCalls = %d, want 0", src.statusCalls)
	}
	// model_list is denied the same way.
	code, _ = callErrCode(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"model_list","arguments":{}}}`)
	if code != float64(ScopeDeniedCode) || src.modelCalls != 0 {
		t.Errorf("model_list denial code = %v, modelCalls = %d, want -32002/0", code, src.modelCalls)
	}
	// provider_list, the granted tool, still works.
	out := call(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`)
	if _, ok := out["result"]; !ok {
		t.Fatalf("granted provider_list denied: %v", out)
	}
	if src.providerCalls != 1 {
		t.Errorf("providerCalls = %d, want 1", src.providerCalls)
	}
}

func TestWildcardGrantCoversReadTools(t *testing.T) {
	srv, src := newCountingServer()
	srv.WithClientIdentity(GatewayKeyClient("key-2", []string{"mcp:read:*"}))
	for _, tool := range []string{"router_status", "provider_list", "model_list"} {
		out := call(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if _, ok := out["result"]; !ok {
			t.Errorf("mcp:read:* grant denied %s: %v", tool, out)
		}
	}
	if src.statusCalls != 1 || src.providerCalls != 1 || src.modelCalls != 1 {
		t.Errorf("dispatch counts = %d/%d/%d, want 1 each", src.statusCalls, src.providerCalls, src.modelCalls)
	}
	// ... but mcp:read:* does NOT cover a manage scope: a
	// read wildcard must never satisfy a manage requirement.
	if chk := CheckScopes([]string{"mcp:read:*"}, []string{ScopeManageRouting, ScopeDestructive}); chk.Allowed {
		t.Errorf("mcp:read:* allowed a manage+destructive requirement, want deny: %+v", chk)
	}
}

func TestAnonymousIdentityDeniedEverything(t *testing.T) {
	srv, src := newCountingServer()
	srv.WithClientIdentity(Anonymous())
	for _, tool := range []string{"router_status", "provider_list", "model_list"} {
		code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if code != float64(ScopeDeniedCode) {
			t.Errorf("anonymous %s code = %v, want %d", tool, code, ScopeDeniedCode)
		}
	}
	if src.statusCalls != 0 || src.providerCalls != 0 || src.modelCalls != 0 {
		t.Errorf("anonymous calls reached dispatch: %d/%d/%d, want 0", src.statusCalls, src.providerCalls, src.modelCalls)
	}
}

func TestHandleWithIdentityPerRequest(t *testing.T) {
	// M9.2 seam: a per-request identity overrides the server
	// default for that call only, without changing the server.
	srv, src := newCountingServer()
	narrow := GatewayKeyClient("key-9", []string{ScopeReadModels})
	raw := srv.HandleWithIdentity(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`), narrow)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if errBody, _ := out["error"].(map[string]any); errBody["code"] != float64(ScopeDeniedCode) {
		t.Fatalf("per-request narrow identity allowed router_status: %v", out)
	}
	if src.statusCalls != 0 {
		t.Errorf("denied per-request call reached dispatch: %d", src.statusCalls)
	}
	// The server default is untouched afterwards — still the
	// fail-closed Anonymous default, so a plain Handle now denies
	// (it no longer silently inherits the local operator).
	out = call(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`)
	if errBody, _ := out["error"].(map[string]any); errBody["code"] != float64(ScopeDeniedCode) {
		t.Errorf("server default identity changed by HandleWithIdentity: %v", out)
	}
	if src.statusCalls != 0 {
		t.Errorf("server-default call after HandleWithIdentity reached dispatch: %d", src.statusCalls)
	}
}

func TestUnknownToolStill32602(t *testing.T) {
	// Unknown tools skip the scope check (they have no required
	// scopes) and keep their -32602 answer.
	srv, _ := newCountingServer()
	srv.WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadProviders}))
	code, msg := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"chat","arguments":{}}}`)
	if code != -32602 {
		t.Errorf("unknown tool code = %v, want -32602", code)
	}
	if !strings.Contains(msg, "chat") {
		t.Errorf("unknown tool message %q does not name the tool", msg)
	}
}

func TestScopeDenialPrecedesGuardrail(t *testing.T) {
	// Order: scope check BEFORE guardrail. A call that would trip
	// BOTH is denied with the scope code, not the guardrail code —
	// the scope decision is the first gate.
	srv, src := newCountingServer()
	srv.WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadProviders}))
	srv.WithGuardrail(guardrails.NewStoreWith(guardrails.Config{BlockSecrets: true}))
	key := "sk-test-secret-VALUE-12345"
	code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"router_status","arguments":{"note":"my key is `+key+`"}}}`)
	if code != float64(ScopeDeniedCode) {
		t.Errorf("scope+guardrail call code = %v, want scope code %d", code, ScopeDeniedCode)
	}
	if src.statusCalls != 0 {
		t.Errorf("denied call reached dispatch: %d", src.statusCalls)
	}
}

func TestDispatchAuditsAllowAndDeny(t *testing.T) {
	log, _ := tempAuditLog(t)
	srv, _ := newCountingServer()
	srv.WithAuditLog(log)
	srv.WithClientIdentity(GatewayKeyClient("key-3", []string{ScopeReadProviders}))
	// Allowed call.
	out := call(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"provider_list","arguments":{"note":"hello"}}}`)
	if _, ok := out["result"]; !ok {
		t.Fatalf("allowed call failed: %v", out)
	}
	// Denied call (scope).
	code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`)
	if code != float64(ScopeDeniedCode) {
		t.Fatalf("expected scope denial, got %v", code)
	}
	// Guardrail-denied call (granted scope, blocked args).
	srv.WithGuardrail(guardrails.NewStoreWith(guardrails.Config{BlockSecrets: true}))
	key := "sk-test-secret-VALUE-12345"
	code, _ = callErrCode(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"provider_list","arguments":{"note":"my key is `+key+`"}}}`)
	if code != float64(guardrails.PolicyRefusalCode) {
		t.Fatalf("expected guardrail refusal, got %v", code)
	}
	// Unknown-tool call.
	code, _ = callErrCode(t, srv, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"chat","arguments":{}}}`)
	if code != -32602 {
		t.Fatalf("expected -32602, got %v", code)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != 4 {
		t.Fatalf("audit entries = %d, err %v, want 4 (allow + 3 denies)", len(entries), err)
	}
	// List is newest-first: reverse to call order.
	byCall := []AuditEntry{entries[3], entries[2], entries[1], entries[0]}
	if byCall[0].Decision != AuditAllow || byCall[0].Tool != "provider_list" {
		t.Errorf("entry 0 = %+v, want allow/provider_list", byCall[0])
	}
	if byCall[1].Decision != AuditDeny || byCall[1].ErrorCode != ScopeDeniedCode {
		t.Errorf("entry 1 = %+v, want deny/-32002", byCall[1])
	}
	if byCall[2].Decision != AuditDeny || byCall[2].ErrorCode != guardrails.PolicyRefusalCode {
		t.Errorf("entry 2 = %+v, want deny/-32001", byCall[2])
	}
	if byCall[3].Decision != AuditDeny || byCall[3].ErrorCode != -32602 {
		t.Errorf("entry 3 = %+v, want deny/-32602", byCall[3])
	}
	// Every entry carries the gateway-key caller id and a hash;
	// no entry contains the raw secret.
	for i, e := range byCall {
		if e.CallerID != "key-3" || e.CallerKind != KindGatewayKey {
			t.Errorf("entry %d caller = %s/%s, want gateway-key/key-3", i, e.CallerKind, e.CallerID)
		}
		if e.InputHash == "" {
			t.Errorf("entry %d has no input hash", i)
		}
		if e.InputHash == key {
			t.Errorf("entry %d stored the raw secret as its hash", i)
		}
	}
	if n := len([]rune(byCall[0].OutputSummary)); n == 0 || n > MaxOutputSummaryChars {
		t.Errorf("allowed entry output summary length = %d, want 1..200", n)
	}
	if byCall[1].OutputSummary != "" || byCall[2].OutputSummary != "" {
		t.Errorf("denied entries carry output summaries, want empty")
	}
}

func TestGatewayKeyIdentityCopiesScopes(t *testing.T) {
	scopes := []string{ScopeReadProviders}
	id := GatewayKeyClient("key-4", scopes)
	scopes[0] = ScopeDestructive // mutate the caller's slice
	if id.HasScope(ScopeDestructive) || !id.HasScope(ScopeReadProviders) {
		t.Errorf("mutating the source slice changed the identity: %+v", id.Scopes)
	}
	// The identity never carries key material: only the id.
	if strings.Contains(id.ID, "sk-") {
		t.Errorf("identity id looks like key material: %q", id.ID)
	}
}
