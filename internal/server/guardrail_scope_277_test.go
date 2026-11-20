// Per-key guardrail scoping, server half (CHECKLIST 277 (d)):
// a key with a non-empty guardrails list runs only its listed
// guardrails on every surface, and the feed reports the scope.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// scopedTestServer builds a server with two gateway keys: one
// scoped to ["pii"] only, one unscoped (global blocking config),
// and blocking on for PII + secrets. It returns the server and
// the two raw keys.
func scopedTestServer(t *testing.T, chatResp provider.ChatResponse) (*Server, string, string, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	scoped, scopedRaw, err := config.IssueKey("scoped", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	scoped.Guardrails = []string{guardrails.KindPII}
	if err := cfg.AddGatewayKey(scoped); err != nil {
		t.Fatal(err)
	}
	global, globalRaw, err := config.IssueKey("global", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(global); err != nil {
		t.Fatal(err)
	}
	store := guardrails.NewStoreWith(guardrails.Config{BlockPII: true, BlockSecrets: true})
	srv := newTestServer(t, &fakeEngine{chatResp: chatResp},
		WithConfig(cfg), WithGuardrails(store))
	return srv, scopedRaw, globalRaw, scoped.ID
}

func chatBodyWithText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"model":    "m",
		"messages": []map[string]string{{"role": "user", "content": text}},
	})
	return string(b)
}

// TestPerKeyGuardrailScoping proves the 245 limitation is gone:
// a key scoped to ["pii"] is NOT blocked by a secret in its
// request (the secret guardrail does not run for that key), while
// the same request under an unscoped key is blocked — and a PII
// request under the scoped key IS blocked (its listed guardrail
// runs).
func TestPerKeyGuardrailScoping(t *testing.T) {
	srv, scopedRaw, globalRaw, _ := scopedTestServer(t, provider.ChatResponse{Content: "hi"})
	// Fake test-fixture key shape (repeated characters — never a
	// real credential).
	secretText := "my key is sk-proj-xxxxxxxxxxxxxxxxxxxx"
	rec := doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText(secretText), scopedRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret text + scoped key: status = %d, want 200 (the secret guardrail does not run for this key): %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText(secretText), globalRaw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("secret text + unscoped key: status = %d, want 403 (the global secret guardrail runs): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32001") {
		t.Fatalf("unscoped refusal body = %s, want the -32001 policy-refusal code", rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText("email me at a@b.com"), scopedRaw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("PII text + scoped key: status = %d, want 403 (its listed guardrail runs): %s", rec.Code, rec.Body.String())
	}
}

// TestPerKeyGuardrailScopingResponse proves the scope also
// applies to the response stage: a provider reply echoing a
// secret is delivered to the scoped key (its list has no secret
// guardrail) and withheld from the unscoped key.
func TestPerKeyGuardrailScopingResponse(t *testing.T) {
	srv, scopedRaw, globalRaw, _ := scopedTestServer(t,
		provider.ChatResponse{Content: "here is the key sk-proj-xxxxxxxxxxxxxxxxxxxx"})
	rec := doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText("hello"), scopedRaw)
	if rec.Code != http.StatusOK {
		t.Fatalf("secret reply + scoped key: status = %d, want 200 (delivered): %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText("hello"), globalRaw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("secret reply + unscoped key: status = %d, want 403 (withheld): %s", rec.Code, rec.Body.String())
	}
}

// TestPerKeyGuardrailScopingFeed proves the guardrails feed
// reports the scope with real counts: 1 of 2 keys scoped, the
// scoped key's list, and the scoped evaluations since start.
func TestPerKeyGuardrailScopingFeed(t *testing.T) {
	srv, scopedRaw, _, scopedID := scopedTestServer(t, provider.ChatResponse{Content: "hi"})
	// Two scoped evaluations (one allowed, one blocked).
	_ = doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText("email me at a@b.com"), scopedRaw)
	_ = doReq(t, srv, "POST", "/v1/chat/completions", chatBodyWithText("hello"), scopedRaw)
	rec := doReq(t, srv, "GET", "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/guardrails: status = %d", rec.Code)
	}
	var f guardrailsFeed
	if err := json.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("decode guardrails feed: %v", err)
	}
	ks := f.KeyScoping
	if ks.ScopedKeys != 1 || ks.TotalKeys != 2 {
		t.Fatalf("key_scoping = %+v, want scoped_keys=1 total_keys=2", ks)
	}
	if len(ks.Keys) != 1 || ks.Keys[0].ID != scopedID || len(ks.Keys[0].Guardrails) != 1 || ks.Keys[0].Guardrails[0] != guardrails.KindPII {
		t.Fatalf("key_scoping.keys = %+v, want the one scoped key with [pii]", ks.Keys)
	}
	if ks.ScopedEvals < 2 {
		t.Fatalf("scoped_evals = %d, want >= 2", ks.ScopedEvals)
	}
}

// TestDashboardKeyScopingAndVertexElements pins the two new
// dashboard surfaces: the per-key scoping card in the Guardrails
// view and the Vertex line in the Providers view. Both render
// from live feeds; neither invents a Playground.
func TestDashboardKeyScopingAndVertexElements(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`id="grKeyScoping"`,
		`id="provVertexLine"`,
		"Per-key guardrail scoping",
		"Vertex AI providers",
		"key_scoping",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	if strings.Contains(body, "Playground") {
		t.Fatal("dashboard must not contain Playground")
	}
}
