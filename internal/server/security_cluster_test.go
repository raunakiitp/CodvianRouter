// Security-cluster handler tests (CHECKLIST 245-246): Vertex
// overrides and unknown guardrail names fail loudly end to end,
// deployment pins outrank client body fields, and both refusals
// surface as real counts on the guard feed.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// capturingEngine wraps fakeEngine and records the last chat
// request the engine received, so a test can prove which values
// actually reached the engine (after deployment pins applied).
type capturingEngine struct {
	*fakeEngine
	last provider.ChatRequest
}

func (c *capturingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.last = req
	return c.fakeEngine.Chat(ctx, req)
}

func securityChatBody(t *testing.T, extra map[string]any) string {
	t.Helper()
	body := map[string]any{
		"model":    "priced-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}
	for k, v := range extra {
		body[k] = v
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal chat body: %v", err)
	}
	return string(b)
}

func TestVertexOverrideRejectedOnChat(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	for _, field := range []string{"vertex_location", "vertex_project", "vertex_ai_location", "vertex_ai_project"} {
		rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", securityChatBody(t, map[string]any{field: "us-east1"}), s.raw)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400: %s", field, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s: body = %s, want it to name the field", field, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "us-east1") {
			t.Errorf("%s: body echoes the rejected value", field)
		}
	}
	// The engine was never called for any of them.
	if s.eng.chatCalls.Load() != 0 {
		t.Errorf("engine chat calls = %d, want 0 (rejected before any provider is dialled)", s.eng.chatCalls.Load())
	}
	// The guard feed counts all four refusals, by reason label.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/guard", "", "")
	var feed guardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode guard feed: %v: %s", err, rec.Body.String())
	}
	if got := feed.Status.SecurityRefusals[guardrails.SecurityRefusalVertexOverride]; got != 4 {
		t.Errorf("guard feed vertex-override refusals = %d, want 4", got)
	}
	// The same refusals show on the guardrails feed too.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	var st guardrails.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode guardrails feed: %v: %s", err, rec.Body.String())
	}
	if got := st.SecurityRefusals[guardrails.SecurityRefusalVertexOverride]; got != 4 {
		t.Errorf("guardrails feed vertex-override refusals = %d, want 4", got)
	}
}

func TestVertexOverrideAllowedWhenAdminOptsIn(t *testing.T) {
	s := newKeyBudgetSetup(t)
	s.cfg.AllowClientVertexOverride = true
	srv := s.server(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", securityChatBody(t, map[string]any{"vertex_location": "us-east1"}), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admin opted in): %s", rec.Code, rec.Body.String())
	}
	if s.eng.chatCalls.Load() != 1 {
		t.Errorf("engine chat calls = %d, want 1", s.eng.chatCalls.Load())
	}
}

func TestDeploymentParamsOutrankClientBody(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// The deployment pins temperature 0.2 for priced-model.
	temp := 0.2
	s.cfg.Providers = []config.ProviderConfig{{
		ID:      "p1",
		BaseURL: "http://127.0.0.1:9",
		Models: []provider.Model{{
			ID:          "priced-model",
			FixedParams: &provider.ModelFixedParams{Temperature: &temp},
		}},
	}}
	cap := &capturingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}}}
	srv := newTestServer(t, cap, WithConfig(s.cfg), WithLedger(s.led), WithPriceBook(s.book))
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", securityChatBody(t, map[string]any{"temperature": 0.9}), s.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cap.last.Temperature != 0.2 {
		t.Errorf("engine saw temperature = %v, want 0.2 (admin pin wins over client 0.9)", cap.last.Temperature)
	}
	if got := rec.Header().Get("X-Codvian-Deployment-Override"); got != "temperature" {
		t.Errorf("X-Codvian-Deployment-Override = %q, want \"temperature\"", got)
	}

	// The other way: no pin — the client's value reaches the engine untouched.
	s2 := newKeyBudgetSetup(t)
	cap2 := &capturingEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "hi", Usage: provider.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}}}
	srv2 := newTestServer(t, cap2, WithConfig(s2.cfg), WithLedger(s2.led), WithPriceBook(s2.book))
	rec = doReq(t, srv2, http.MethodPost, "/v1/chat/completions", securityChatBody(t, map[string]any{"temperature": 0.9}), s2.raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cap2.last.Temperature != 0.9 {
		t.Errorf("engine saw temperature = %v, want 0.9 (unpinned field keeps the client value)", cap2.last.Temperature)
	}
	if got := rec.Header().Get("X-Codvian-Deployment-Override"); got != "" {
		t.Errorf("X-Codvian-Deployment-Override = %q, want empty (nothing pinned)", got)
	}
}

func TestUnknownGuardrailNameRejectedOnKeyUpdate(t *testing.T) {
	s := newKeyBudgetSetup(t)
	// Key updates are admin-scoped (M1.1): authenticate as the
	// admin key, not the chat-only fixture key (see adminKeyFor).
	adminRaw := adminKeyFor(t, s)
	srv := s.server(t)
	// A typo'd guardrail name fails loudly, naming the name.
	rec := doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"guardrails":["pii","toxicty-screen"]}`, adminRaw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "toxicty-screen") {
		t.Errorf("body = %s, want it to name the unknown guardrail", rec.Body.String())
	}
	// The key was not changed by the rejected write.
	key, _ := s.cfg.FindGatewayKey(s.id)
	if len(key.Guardrails) != 0 {
		t.Errorf("key guardrails = %v after rejected write, want empty", key.Guardrails)
	}
	// The refusal is counted on the guard feed.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/guard", "", "")
	var feed guardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode guard feed: %v: %s", err, rec.Body.String())
	}
	if got := feed.Status.SecurityRefusals[guardrails.SecurityRefusalUnknownGuardrail]; got != 1 {
		t.Errorf("guard feed unknown-guardrail refusals = %d, want 1", got)
	}
	// A registered name is accepted and shows in the key view.
	rec = doReq(t, srv, http.MethodPost, "/admin/keys/"+s.id, `{"guardrails":["pii","secret"]}`, adminRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("registered names: status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var upd keyUpdateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &upd); err != nil {
		t.Fatalf("decode key update: %v: %s", err, rec.Body.String())
	}
	if len(upd.Key.Guardrails) != 2 || upd.Key.Guardrails[0] != "pii" || upd.Key.Guardrails[1] != "secret" {
		t.Errorf("key view guardrails = %v, want [pii secret]", upd.Key.Guardrails)
	}
}

func TestVertexOverrideRejectedOnResponsesAndMessages(t *testing.T) {
	s := newKeyBudgetSetup(t)
	srv := s.server(t)
	rec := doReq(t, srv, http.MethodPost, "/v1/responses", `{"model":"priced-model","input":"hi","vertex_project":"my-project"}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("responses: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vertex_project") {
		t.Errorf("responses body = %s, want it to name the field", rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodPost, "/v1/messages", `{"model":"priced-model","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"vertex_location":"us-east1"}`, s.raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("messages: status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "vertex_location") {
		t.Errorf("messages body = %s, want it to name the field", rec.Body.String())
	}
}
