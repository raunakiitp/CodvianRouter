package provider

// A1 gap 3: per-provider quirk table + executor dispatch. Every quirk in
// the table (internal/provider/quirks.go) is proven twice here:
//   - against a mock upstream (httptest), proving the dispatched client
//     actually sends the quirked request (path, query, body);
//   - as a fails-when-disabled check: the mock tests resolve the quirks
//     through quirksForBaseURL — the same lookup NewClient dispatches
//     through — so an emptied table leaves the client on the generic
//     /v1 paths and the assertions fail.
// A control test proves a provider with NO quirks is untouched, and a
// guard test proves native-wire clients never receive quirks.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// quirkChatMock serves a plain JSON chat completion, recording the
// request path, query and body.
type quirkChatMock struct {
	path  string
	query string
	body  []byte
}

func newQuirkChatMock(t *testing.T) (*httptest.Server, *quirkChatMock) {
	t.Helper()
	rec := &quirkChatMock{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		rec.query = r.URL.RawQuery
		rec.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-q","model":"m","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// applyQuirksForHost is the test-side dispatch: it resolves the quirk
// table for a real documented upstream host and applies the result to a
// client built against the mock upstream. The lookup is exactly what
// NewClient runs (quirksForBaseURL), so an emptied table resolves nothing
// and the client below keeps the generic paths — the mock assertions then
// fail, which is the fails-when-disabled property of these tests.
func applyQuirksForHost(t *testing.T, c Client, documentedBase string) {
	t.Helper()
	if qs, ok := quirksForBaseURL(documentedBase); ok {
		c.(*openAIClient).quirks = qs
	}
}

func quirkChatReq() ChatRequest {
	return ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}
}

// TestQuirksTableCoversDocumentedSurfaces pins the table contents: each
// documented provider base resolves the quirks recorded from its catalog
// entry and docs. Fails when the table is emptied or an entry loses a
// quirk.
func TestQuirksTableCoversDocumentedSurfaces(t *testing.T) {
	cases := []struct {
		provider string
		base     string
		want     quirkSet
	}{
		{"deepseek", "https://api.deepseek.com", quirkSet{NoV1Insert: true}},
		{"perplexity", "https://api.perplexity.ai", quirkSet{NoV1Insert: true}},
		{"qianfan", "https://qianfan.baidubce.com/v2", quirkSet{NoV1Insert: true}},
		{"bigmodel", "https://open.bigmodel.cn/api/paas/v4", quirkSet{NoV1Insert: true}},
		{"glm", "https://api.z.ai/api/coding/paas/v4", quirkSet{NoV1Insert: true}},
		{"doubao", "https://ark.cn-beijing.volces.com/api/v3", quirkSet{NoV1Insert: true}},
		// Region host variants: the Ark docs document region hosts, so the
		// suffix rule must match them too.
		{"doubao region", "https://ark.ap-southeast.volces.com/api/v3", quirkSet{NoV1Insert: true}},
		// Legacy Azure resource root (no /openai/v1 path): the deployment-
		// scoped path quirk applies.
		{"azure-openai legacy", "https://myresource.openai.azure.com", quirkSet{AzureLegacyDeploymentPath: true}},
	}
	for _, tc := range cases {
		got, ok := quirksForBaseURL(tc.base)
		if !ok {
			t.Errorf("%s (%s): quirk table resolved no entry — the documented quirk is missing", tc.provider, tc.base)
			continue
		}
		if got != tc.want {
			t.Errorf("%s (%s): quirks = %+v, want %+v", tc.provider, tc.base, got, tc.want)
		}
	}
}

// TestQuirksTableRejectsUndocumentedHosts proves matching is scoped to
// the documented hosts: other hosts — including the catalog's own
// Foundry-style azure base — resolve no quirks.
func TestQuirksTableRejectsUndocumentedHosts(t *testing.T) {
	cases := []string{
		"https://api.openai.com",
		"https://api.x.ai/v1",
		"https://api.groq.com/openai/v1",
		// A proxy mirroring its own path layout: a catalog base-URL
		// override must not inherit the upstream's quirks.
		"https://my-proxy.example.com/v1",
		// Foundry /openai/v1 needs no deployment path and no api-version.
		"https://myresource.openai.azure.com/openai/v1",
	}
	for _, base := range cases {
		if qs, ok := quirksForBaseURL(base); ok {
			t.Errorf("%s: unexpectedly resolved quirks %+v", base, qs)
		}
	}
}

// TestQuirkNoV1InsertURLs proves the dispatched client builds the
// documented paths on the real catalog bases: the API segment is appended
// verbatim, never behind an inserted /v1 (joinAPIPath would 404 these).
func TestQuirkNoV1InsertURLs(t *testing.T) {
	cases := []struct {
		provider string
		base     string
		chat     string
		models   string
	}{
		{"deepseek", "https://api.deepseek.com",
			"https://api.deepseek.com/chat/completions", "https://api.deepseek.com/models"},
		{"perplexity", "https://api.perplexity.ai",
			"https://api.perplexity.ai/chat/completions", "https://api.perplexity.ai/models"},
		{"qianfan", "https://qianfan.baidubce.com/v2",
			"https://qianfan.baidubce.com/v2/chat/completions", "https://qianfan.baidubce.com/v2/models"},
		{"bigmodel", "https://open.bigmodel.cn/api/paas/v4",
			"https://open.bigmodel.cn/api/paas/v4/chat/completions", "https://open.bigmodel.cn/api/paas/v4/models"},
		{"glm", "https://api.z.ai/api/coding/paas/v4",
			"https://api.z.ai/api/coding/paas/v4/chat/completions", "https://api.z.ai/api/coding/paas/v4/models"},
		{"doubao", "https://ark.cn-beijing.volces.com/api/v3",
			"https://ark.cn-beijing.volces.com/api/v3/chat/completions", "https://ark.cn-beijing.volces.com/api/v3/models"},
	}
	for _, tc := range cases {
		c, err := NewClient(tc.base, "k")
		if err != nil {
			t.Fatalf("%s: NewClient: %v", tc.provider, err)
		}
		oc := c.(*openAIClient)
		if got := oc.chatURL(); got != tc.chat {
			// When the quirk table is emptied, NewClient dispatches no
			// quirks and this still reads .../v1/chat/completions — the
			// documented path is not built and the test fails.
			t.Errorf("%s: chatURL = %q, want %q", tc.provider, got, tc.chat)
		}
		if got := oc.modelsURL(); got != tc.models {
			t.Errorf("%s: modelsURL = %q, want %q", tc.provider, got, tc.models)
		}
	}
}

// TestQuirkNoV1InsertChatRequest proves the quirk reaches the wire: the
// dispatched client's chat request lands on the documented path (no
// /v1) with the auth intact.
func TestQuirkNoV1InsertChatRequest(t *testing.T) {
	srv, rec := newQuirkChatMock(t)
	c, err := NewClient(srv.URL, "sk-test", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	applyQuirksForHost(t, c, "https://api.deepseek.com")
	if _, err := c.Chat(context.Background(), quirkChatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if rec.path != "/chat/completions" {
		// Table emptied → the generic dispatch sends /v1/chat/completions
		// and this fails, which is the fails-when-disabled property.
		t.Errorf("chat path = %q, want /chat/completions (no /v1 inserted)", rec.path)
	}
	if got := rec.body; !strings.Contains(string(got), `"model":"m"`) {
		t.Errorf("body lost the model field: %s", got)
	}
}

// TestQuirkNoV1InsertChatStreamRequest proves the streaming executor
// dispatches the quirk too (ChatStreamChunks shares chatURLForModel).
func TestQuirkNoV1InsertChatStreamRequest(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	var rec quirkChatMock
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "sk-test", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	applyQuirksForHost(t, c, "https://qianfan.baidubce.com/v2")
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("client does not implement ChunkStreamer")
	}
	if _, err := cs.ChatStreamChunks(context.Background(), quirkChatReq(), nil); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if rec.path != "/chat/completions" {
		t.Errorf("stream path = %q, want /chat/completions (no /v1 inserted)", rec.path)
	}
}

// TestQuirkNoV1InsertModelsRequest proves the add-time key test /
// model list also land on the documented path (ListModels is the first
// call a provider add makes, so a wrong path fails the add outright).
func TestQuirkNoV1InsertModelsRequest(t *testing.T) {
	var rec quirkChatMock
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "sk-test", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	applyQuirksForHost(t, c, "https://ark.cn-beijing.volces.com/api/v3")
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if rec.path != "/models" {
		t.Errorf("models path = %q, want /models (no /v1 inserted)", rec.path)
	}
}

// TestQuirkAzureLegacyDeploymentPath proves the legacy Azure surface
// dispatches the deployment-scoped executor: chat lands on
// /openai/deployments/{deployment}/chat/completions with the GA
// api-version query, and the body carries no model field (the
// deployment IS the model, per the documented legacy request shape).
func TestQuirkAzureLegacyDeploymentPath(t *testing.T) {
	srv, rec := newQuirkChatMock(t)
	c, err := NewClient(srv.URL, "az-key", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	applyQuirksForHost(t, c, "https://myresource.openai.azure.com")
	req := ChatRequest{Model: "gpt-4o-deploy", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if rec.path != "/openai/deployments/gpt-4o-deploy/chat/completions" {
		t.Errorf("chat path = %q, want /openai/deployments/gpt-4o-deploy/chat/completions", rec.path)
	}
	if rec.query != "api-version="+azureLegacyAPIVersion {
		t.Errorf("query = %q, want api-version=%s", rec.query, azureLegacyAPIVersion)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.body, &body); err != nil {
		t.Fatalf("body not JSON: %v (%s)", err, rec.body)
	}
	if _, has := body["model"]; has {
		t.Errorf("legacy deployment body still carries the model field: %s", rec.body)
	}
	if _, has := body["messages"]; !has {
		t.Errorf("legacy deployment body lost the messages field: %s", rec.body)
	}
}

// TestAzureFoundryV1BaseUnquirked proves the guard: a Foundry /openai/v1
// base dispatches NO azure quirk — standard path, no api-version query,
// model stays in the body.
func TestAzureFoundryV1BaseUnquirked(t *testing.T) {
	c, err := NewClient("https://myresource.openai.azure.com/openai/v1", "az-key")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc := c.(*openAIClient)
	if oc.quirks.AzureLegacyDeploymentPath {
		t.Fatal("/openai/v1 base dispatched the legacy deployment quirk")
	}
	want := "https://myresource.openai.azure.com/openai/v1/chat/completions"
	if got := oc.chatURLForModel("gpt-4o"); got != want {
		t.Errorf("chatURLForModel = %q, want %q", got, want)
	}
	payload, err := oc.chatPayloadForQuirks(&chatCompletionRequest{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("chatPayloadForQuirks: %v", err)
	}
	if !strings.Contains(string(payload), `"model":"gpt-4o"`) {
		t.Errorf("payload lost the model field: %s", payload)
	}
}

// TestProviderWithoutQuirksUntouched is the control: a provider with no
// documented quirks keeps the generic /v1 paths, the Bearer auth and the
// model in the body — byte-for-byte the pre-quirk behavior.
func TestProviderWithoutQuirksUntouched(t *testing.T) {
	srv, rec := newQuirkChatMock(t)
	c, err := NewClient(srv.URL, "sk-test", WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// A quirk-free host resolves nothing — the client stays untouched.
	if qs, ok := quirksForBaseURL("https://api.x.ai/v1"); ok {
		t.Fatalf("api.x.ai unexpectedly resolved quirks %+v", qs)
	}
	req := quirkChatReq()
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if rec.path != "/v1/chat/completions" {
		t.Errorf("chat path = %q, want the generic /v1/chat/completions", rec.path)
	}
	if rec.query != "" {
		t.Errorf("query = %q, want none", rec.query)
	}
	if !strings.Contains(string(rec.body), `"model":"m"`) {
		t.Errorf("body lost the model field: %s", rec.body)
	}
	// The generic URL builders on real quirk-free bases are unchanged.
	for _, tc := range []struct{ base, chat string }{
		{"https://api.openai.com", "https://api.openai.com/v1/chat/completions"},
		// A base that already ends in /v1 is used as-is (joinAPIPath).
		{"https://api.x.ai/v1", "https://api.x.ai/v1/chat/completions"},
	} {
		oc, err := NewClient(tc.base, "k")
		if err != nil {
			t.Fatalf("NewClient(%s): %v", tc.base, err)
		}
		oo := oc.(*openAIClient)
		if got := oo.chatURL(); got != tc.chat {
			t.Errorf("%s: chatURL = %q, want %q", tc.base, got, tc.chat)
		}
	}
}

// TestQuirksNeverApplyToNativeWire is the guard: the wire selection
// returns a native-wire wrapper before the quirk consult, so its
// embedded quirks stay zero and its own URL builders are untouched.
func TestQuirksNeverApplyToNativeWire(t *testing.T) {
	c, err := NewClient("https://api.deepseek.com", "k", WithWireFormat(WireFormatAnthropic))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	oc, ok := c.(*anthropicClient)
	if !ok {
		t.Fatalf("wire anthropic returned %T, want *anthropicClient", c)
	}
	if oc.quirks != (quirkSet{}) {
		t.Errorf("native-wire client received quirks %+v", oc.quirks)
	}
	if got := oc.messagesURL(); got != "https://api.deepseek.com/v1/messages" {
		t.Errorf("messagesURL = %q, want the /v1 Messages path", got)
	}
}
