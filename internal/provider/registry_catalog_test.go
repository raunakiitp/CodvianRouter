package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// recordingUpstream serves /v1/models, /v1/chat/completions,
// /v1/messages, the Gemini shapes (/models,
// /models/{model}:generateContent), the Cohere shape (/v2/chat),
// and the Bedrock shapes (/foundation-models,
// /model/{id}/converse), and records every request's path and
// headers, so tests can prove which auth the client actually sent
// and which wire it spoke.
type recordingUpstream struct {
	mu           sync.Mutex
	models       string
	chatBody     string
	messagesBody string
	geminiModels string
	geminiBody   string
	cohereBody   string
	bedrockBody  string
	seen         []recordedRequest
}

type recordedRequest struct {
	path    string
	headers http.Header
}

func (m *recordingUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.seen = append(m.seen, recordedRequest{path: r.URL.Path, headers: r.Header.Clone()})
		m.mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.models))
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.chatBody))
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.messagesBody))
		case "/v2/chat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.cohereBody))
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.geminiModels))
		case "/foundation-models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"modelSummaries":[{"modelId":"anthropic.claude-3-haiku-20240307-v1:0"},{"modelId":"meta.llama3-70b-instruct-v1:0"}]}`))
		default:
			if strings.HasSuffix(r.URL.Path, ":generateContent") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(m.geminiBody))
				return
			}
			if strings.HasSuffix(r.URL.Path, "/converse") {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(m.bedrockBody))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (m *recordingUpstream) requests() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedRequest, len(m.seen))
	copy(out, m.seen)
	return out
}

// lastHeader returns the value of header on the most recent request to
// path, and whether any such request carried it.
func (m *recordingUpstream) lastHeader(path, header string) (string, bool) {
	for _, req := range m.requests() {
		if req.path == path {
			v := req.headers.Get(header)
			return v, v != ""
		}
	}
	return "", false
}

func newRecordingServer(t *testing.T) (*httptest.Server, *recordingUpstream) {
	t.Helper()
	m := &recordingUpstream{
		models:       `{"data":[{"id":"model-a"},{"id":"model-b"}]}`,
		chatBody:     `{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		messagesBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hi"}],"model":"model-a","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`,
		geminiModels: `{"models":[{"name":"models/gemini-2.0-flash"},{"name":"models/gemini-1.5-pro"}]}`,
		geminiBody:   `{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"totalTokenCount":15},"modelVersion":"gemini-2.0-flash"}`,
		cohereBody:   `{"id":"chat-1","finish_reason":"COMPLETE","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"usage":{"input_tokens":10,"output_tokens":5}}`,
		bedrockBody:  `{"output":{"message":{"role":"assistant","content":[{"text":"hi"}]}},"stopReason":"end_turn","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}}`,
	}
	return httptest.NewServer(m.handler()), m
}

func newCatalogTestRegistry(t *testing.T) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	return reg, dir
}

func TestRegistryAddCatalogGroq(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, dir := newCatalogTestRegistry(t)

	p, err := reg.AddCatalogWithOptions(context.Background(), "groq", "sk-groq-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog groq: %v", err)
	}
	if p.CatalogID != "groq" {
		t.Errorf("CatalogID = %q, want groq", p.CatalogID)
	}
	if p.Verification != "verified-from-docs" {
		t.Errorf("Verification = %q, want verified-from-docs", p.Verification)
	}
	if p.WireFormat != WireFormatOpenAI {
		t.Errorf("WireFormat = %q, want openai", p.WireFormat)
	}
	if p.Name != "Groq" {
		t.Errorf("Name = %q, want Groq", p.Name)
	}
	if p.BaseURL != srv.URL {
		t.Errorf("BaseURL = %q, want mock URL", p.BaseURL)
	}
	if p.AuthEndpoint != "chat" {
		t.Errorf("AuthEndpoint = %q, want chat", p.AuthEndpoint)
	}
	if len(p.Models) != 2 {
		t.Errorf("Models = %d, want 2", len(p.Models))
	}

	// The request path sends the catalog-resolved auth (Bearer here)
	// and the key decrypts back.
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); !ok || got != "Bearer sk-groq-test" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-groq-test", got, ok)
	}

	// Persisted: catalog fields survive a reload.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("reload vault: %v", err)
	}
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.Get(p.ID)
	if !ok {
		t.Fatal("provider not persisted")
	}
	if got.CatalogID != "groq" || got.Verification != "verified-from-docs" || got.AuthEndpoint != "chat" {
		t.Errorf("reloaded catalog fields = %+v", got)
	}
}

func TestRegistryAddCatalogFireworksAltHeader(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	p, err := reg.AddCatalogWithOptions(context.Background(), "fireworks", "fw-test-key",
		CatalogAddOptions{Endpoint: "chat", Variant: "alt-header", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog fireworks alt-header: %v", err)
	}
	if p.AuthVariant != "alt-header" {
		t.Errorf("AuthVariant = %q, want alt-header", p.AuthVariant)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "X-Fireworks-Api-Key"); !ok || got != "fw-test-key" {
		t.Errorf("X-Fireworks-Api-Key = %q (present=%v), want fw-test-key", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); ok {
		t.Errorf("Authorization must NOT be sent for the alt-header variant, got %q", got)
	}
}

func TestRegistryAddCatalogDeepSeekAnthropicEndpoint(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// DeepSeek dual-endpoint quirk: same key, Bearer on the OpenAI
	// surface, x-api-key on the Anthropic surface.
	p, err := reg.AddCatalogWithOptions(context.Background(), "deepseek", "sk-ds-test",
		CatalogAddOptions{Endpoint: "anthropic", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog deepseek/anthropic: %v", err)
	}
	if p.AuthEndpoint != "anthropic" {
		t.Errorf("AuthEndpoint = %q, want anthropic", p.AuthEndpoint)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	// The Anthropic surface speaks Messages, not OpenAI chat: the
	// request must land on /v1/messages with x-api-key, and the
	// Anthropic reply must come back as an OpenAI-shaped response.
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q, want stop", resp.FinishReason)
	}
	if got, ok := mock.lastHeader("/v1/messages", "x-api-key"); !ok || got != "sk-ds-test" {
		t.Errorf("x-api-key = %q (present=%v), want sk-ds-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/messages", "Authorization"); ok {
		t.Errorf("Authorization must NOT be sent on the Anthropic surface, got %q", got)
	}
}

func TestRegistryAddCatalogAnthropicMessagesEndpoint(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// Anthropic quirk: Bearer is now preferred AND anthropic-version
	// (2026-10-21) is REQUIRED on the messages surface.
	p, err := reg.AddCatalogWithOptions(context.Background(), "anthropic", "sk-ant-test",
		CatalogAddOptions{Endpoint: "messages", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog anthropic/messages: %v", err)
	}
	if p.AuthEndpoint != "messages" {
		t.Errorf("AuthEndpoint = %q, want messages", p.AuthEndpoint)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	// The messages surface speaks Anthropic Messages: the request
	// must land on /v1/messages with Bearer + anthropic-version.
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	if got, ok := mock.lastHeader("/v1/messages", "Authorization"); !ok || got != "Bearer sk-ant-test" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-ant-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/messages", "anthropic-version"); !ok || got != "2026-10-21" {
		t.Errorf("anthropic-version = %q (present=%v), want 2026-10-21", got, ok)
	}
}

func TestRegistryAddCatalogGeminiGenerateEndpoint(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// Gemini's only recorded surface is "generate" with the
	// x-goog-api-key header; the default endpoint resolves to it.
	p, err := reg.AddCatalogWithOptions(context.Background(), "gemini", "gem-key-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog gemini: %v", err)
	}
	if p.AuthEndpoint != "generate" {
		t.Errorf("AuthEndpoint = %q, want generate", p.AuthEndpoint)
	}
	if p.WireFormat != WireFormatGemini {
		t.Errorf("WireFormat = %q, want gemini", p.WireFormat)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "gemini-2.0-flash" {
		t.Errorf("Models = %+v, want the models/ prefix stripped", p.Models)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	// The generate surface speaks Gemini: the request must land on
	// /models/{model}:generateContent with x-goog-api-key and no
	// Authorization header.
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	var genPath string
	var genHeaders http.Header
	for _, req := range mock.requests() {
		if strings.HasSuffix(req.path, ":generateContent") {
			genPath, genHeaders = req.path, req.headers
		}
	}
	if genPath != "/models/gemini-2.0-flash:generateContent" {
		t.Errorf("generate path = %q", genPath)
	}
	if got := genHeaders.Get("x-goog-api-key"); got != "gem-key-test" {
		t.Errorf("x-goog-api-key = %q, want gem-key-test", got)
	}
	if _, ok := genHeaders["Authorization"]; ok {
		t.Errorf("Gemini must not send an Authorization header")
	}
}

func TestRegistryAddCatalogAzureAPIKeyVariant(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// Azure default is Entra exchange (refused); the api-key variant
	// is the static-header surface.
	p, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", "az-test-key",
		CatalogAddOptions{Endpoint: "chat", Variant: "api-key", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog azure-openai/api-key: %v", err)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "api-key"); !ok || got != "az-test-key" {
		t.Errorf("api-key = %q (present=%v), want az-test-key", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); ok {
		t.Errorf("Authorization must NOT be sent for the api-key variant, got %q", got)
	}
}

func TestRegistryAddCatalogRefusals(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	cases := []struct {
		name    string
		id      string
		opts    CatalogAddOptions
		wantSub string
	}{
		{"tos-grey oauth", "claude", CatalogAddOptions{BaseURL: srv.URL}, "ToS-grey"},
		{"tos-grey reference", "chatgpt-web-codex", CatalogAddOptions{BaseURL: srv.URL}, "ToS-grey"},
		{"vertex exchange", "vertex", CatalogAddOptions{BaseURL: srv.URL}, "exchange"},
		{"bedrock exchange", "bedrock", CatalogAddOptions{BaseURL: srv.URL}, "exchange"},
		{"azure entra exchange", "azure-openai", CatalogAddOptions{BaseURL: srv.URL}, "exchange"},
		{"openrouter jwt exchange", "openrouter", CatalogAddOptions{Variant: "jwt", BaseURL: srv.URL}, "exchange"},
		{"unknown id", "no-such-provider", CatalogAddOptions{BaseURL: srv.URL}, "unknown catalog provider"},
		{"no base url", "oneminai", CatalogAddOptions{}, "no known base URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.AddCatalogWithOptions(context.Background(), tc.id, "k", tc.opts)
			if err == nil {
				t.Fatalf("AddCatalog %q: expected error, got nil", tc.id)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestCT3AddCatalogRefusesRetiredIDs — CT-3 registry half: adding a
// retired catalog ID fails with the retirement reason, not a generic
// "unknown catalog provider". The retired check lives in lookupCatalog,
// so it also fires for add-by-alias ("bb" → blackbox) and the
// config-file load path (TestCT3LoadRefusesRetiredID). A genuinely
// unknown ID still reports "unknown catalog provider" with no
// "retired" in it, and the CT-2 removed-provider refusals are
// unaffected (removed and retired are disjoint lists; see
// TestCT2AddCatalogRefusesRemovedIDs).
func TestCT3AddCatalogRefusesRetiredIDs(t *testing.T) {
	reg, _ := newCatalogTestRegistry(t)

	for _, tc := range []struct {
		id      string
		wantSub string
	}{
		{"blackbox", "retired"},
		{"bb", "retired"}, // alias resolves to the same retired entry
		{"galadriel", "retired"},
		{"ai21", "retired"},
		{"bytez", "retired"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			_, err := reg.AddCatalog(context.Background(), tc.id, "k")
			if err == nil {
				t.Fatalf("AddCatalog(%q) succeeded, want a retired-provider error", tc.id)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("AddCatalog(%q) error = %v, want it to say %q", tc.id, err, tc.wantSub)
			}
			if strings.Contains(err.Error(), "unknown catalog provider") {
				t.Errorf("AddCatalog(%q) error = %v, want the retirement reason, not \"unknown catalog provider\"", tc.id, err)
			}
		})
	}

	// The retired reason must name the provider and say what happened.
	t.Run("blackbox reason", func(t *testing.T) {
		_, err := reg.AddCatalog(context.Background(), "blackbox", "k")
		if err == nil {
			t.Fatal("AddCatalog(blackbox) succeeded, want a retired-provider error")
		}
		if !strings.Contains(err.Error(), "Blackbox AI") || !strings.Contains(err.Error(), "404") {
			t.Errorf("AddCatalog(blackbox) error = %v, want it to name the provider and its retirement reason", err)
		}
	})

	// Negative: a genuinely unknown ID still reports "unknown catalog
	// provider" — and must NOT mention retirement.
	t.Run("unknown id is not retired", func(t *testing.T) {
		_, err := reg.AddCatalog(context.Background(), "no-such-provider", "k")
		if err == nil {
			t.Fatal("AddCatalog(no-such-provider) succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "unknown catalog provider") {
			t.Errorf("AddCatalog(no-such-provider) error = %v, want \"unknown catalog provider\"", err)
		}
		if strings.Contains(err.Error(), "retired") {
			t.Errorf("AddCatalog(no-such-provider) error = %v, must not mention retirement", err)
		}
	})
}

// TestCT3LoadRefusesRetiredID — CT-3 registry half, config-file load
// path: a config file naming a retired provider fails at LOAD with
// the retirement reason (resolveCatalogForLoad shares lookupCatalog
// with the add path), not with a generic "unknown catalog provider".
func TestCT3LoadRefusesRetiredID(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)

	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:        "prov-blackbox",
		Name:      "Blackbox AI",
		CatalogID: "blackbox",
		Enabled:   true,
	}})
	if _, err := NewFileRegistry(path, WithCipher(cipher)); err == nil {
		t.Fatal("expected load error for retired catalog blackbox, got nil")
	} else if !strings.Contains(err.Error(), "retired") {
		t.Errorf("load error = %v, want it to say retired", err)
	} else if strings.Contains(err.Error(), "unknown catalog provider") {
		t.Errorf("load error = %v, want the retirement reason, not \"unknown catalog provider\"", err)
	}
}

func TestRegistryAddCatalogTemplatedBaseURL(t *testing.T) {
	reg, _ := newCatalogTestRegistry(t)

	// Azure's catalog base is a {resource} template: without a concrete
	// override the add must refuse and say so.
	_, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", "az-key",
		CatalogAddOptions{Endpoint: "chat", Variant: "api-key"})
	if err == nil {
		t.Fatal("expected error for templated base URL, got nil")
	}
	if !strings.Contains(err.Error(), "templated") {
		t.Errorf("error %q does not mention templated base URL", err.Error())
	}
}

func TestRegistryAddCatalogAlias(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	p, err := reg.AddCatalogWithOptions(context.Background(), "grok", "xai-key",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog grok (alias): %v", err)
	}
	if p.CatalogID != "xai" {
		t.Errorf("CatalogID = %q, want xai (alias resolved to the real ID)", p.CatalogID)
	}
}

// TestCT2AddCatalogOcAliasResolves — CT-2 nit 1 (reviewer b5b37add):
// "oc" is the removed register's alias for opencode
// (catalog/removed.go:98), but the opencode catalog entry is the
// legitimate authenticated console API, and it carried no Alias
// field — so AddCatalogWithOptions("oc") failed with a generic
// "unknown catalog provider" instead of resolving. The entry now
// carries Alias "oc" (catalog/verified.go:587), so the alias
// resolves to the authenticated entry and the add succeeds — it
// is NOT a typed RemovedProviderError, because the removal covers
// only the keyless "OpenCode Free" variant and
// RemovedProvider.Matches (catalog/removed.go:128-139) does not
// match an API-key entry. This pins the scope exception for the
// alias form, mirroring the "opencode authenticated entry
// unaffected" subtest in TestCT2AddCatalogRefusesRemovedIDs.
func TestCT2AddCatalogOcAliasResolves(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	p, err := reg.AddCatalogWithOptions(context.Background(), "oc", "oc-key",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog(oc) = %v, want success (alias resolves to the authenticated opencode console entry)", err)
	}
	if p.CatalogID != "opencode" {
		t.Fatalf("CatalogID = %q, want opencode (alias resolved to the real ID)", p.CatalogID)
	}
}

// writeStoreFile writes provider configs directly to a JSON store file,
// for tests that need a provider in a shape Add would refuse to create
// (e.g. a catalog-linked provider whose client cannot be built yet).
func writeStoreFile(t *testing.T, dir string, cfgs []ProviderConfig) string {
	t.Helper()
	data, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatalf("marshal store: %v", err)
	}
	path := filepath.Join(dir, "providers.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write store: %v", err)
	}
	return path
}

func sealTestKey(t *testing.T, dir, plaintext string) string {
	t.Helper()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	sealed, err := NewVaultCipher(vault).Encrypt(plaintext)
	if err != nil {
		t.Fatalf("seal key: %v", err)
	}
	return sealed
}

func TestRegistryLoadCatalogResolution(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)

	// A config-file provider with a catalog ID and no base URL gets
	// its base URL, wire and verification from the catalog at load.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:        "prov-groq",
		Name:      "Groq",
		BaseURL:   "",
		CatalogID: "groq",
		Enabled:   true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	p, ok := reg.Get("prov-groq")
	if !ok {
		t.Fatal("provider not loaded")
	}
	if p.BaseURL != "https://api.groq.com/openai/v1" {
		t.Errorf("BaseURL = %q, want catalog base URL", p.BaseURL)
	}
	if p.WireFormat != WireFormatOpenAI {
		t.Errorf("WireFormat = %q, want openai", p.WireFormat)
	}
	if p.Verification != "verified-from-docs" {
		t.Errorf("Verification = %q, want verified-from-docs", p.Verification)
	}
	if p.AuthEndpoint != "chat" {
		t.Errorf("AuthEndpoint = %q, want chat", p.AuthEndpoint)
	}
}

func TestRegistryLoadCatalogBadIDs(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)

	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"unknown id", "no-such-provider", "unknown catalog provider"},
		{"tos-grey", "claude", "ToS-grey"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeStoreFile(t, dir, []ProviderConfig{{
				ID:        "prov-x",
				Name:      "X",
				CatalogID: tc.id,
				Enabled:   true,
			}})
			if _, err := NewFileRegistry(path, WithCipher(cipher)); err == nil {
				t.Fatalf("expected load error for catalog %q, got nil", tc.id)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestRegistryLoadRemovedProviders — CT-2 acceptance (3): a config
// file naming a removed provider fails at LOAD
// (resolveCatalogForLoad / Load), not at first request, with a typed
// error naming the removal. Covers the catalog-ID path (each removed
// ID + an alias), the explicit-base-URL path (no catalog link at
// all, and a catalog-linked provider pointed at a removed host),
// and the negatives: the authenticated opencode entry and a
// similar-but-unremoved host load fine.
func TestRegistryLoadRemovedProviders(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)

	load := func(cfgs []ProviderConfig) error {
		path := writeStoreFile(t, dir, cfgs)
		_, err := NewFileRegistry(path, WithCipher(cipher))
		return err
	}
	assertRemoved := func(t *testing.T, err error, wantID string) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected a removed-provider load error, got nil")
		}
		var remErr *catalog.RemovedProviderError
		if !errors.As(err, &remErr) {
			t.Fatalf("load error = %v, want typed *catalog.RemovedProviderError", err)
		}
		if remErr.Removed.ID != wantID {
			t.Fatalf("load removal names %q, want %q", remErr.Removed.ID, wantID)
		}
		if !strings.Contains(err.Error(), "was removed on") {
			t.Fatalf("load error = %v, want it to name the removal", err)
		}
	}

	// Catalog-ID path: each removed ID, plus an alias.
	for _, tc := range []struct {
		catalogID string
		want      string
	}{
		{"puter", "puter"},
		{"pu", "puter"},
		{"theoldllm", "theoldllm"},
		{"gemini-business", "gemini-business"},
		{"suno", "suno"},
	} {
		t.Run("catalog/"+tc.catalogID, func(t *testing.T) {
			assertRemoved(t, load([]ProviderConfig{{
				ID: "prov-x", Name: "X", CatalogID: tc.catalogID, Enabled: true,
			}}), tc.want)
		})
	}

	// Explicit base URL, no catalog link at all.
	t.Run("explicit base URL, no catalog link", func(t *testing.T) {
		assertRemoved(t, load([]ProviderConfig{{
			ID: "prov-x", Name: "X", BaseURL: "https://puter.com/v1", Enabled: true,
		}}), "puter")
	})

	// Catalog-linked provider whose explicit base URL points at a
	// removed host (the catalog ID itself is fine).
	t.Run("catalog-linked, removed base URL override", func(t *testing.T) {
		assertRemoved(t, load([]ProviderConfig{{
			ID: "prov-x", Name: "X", CatalogID: "groq",
			BaseURL: "https://studio-api.suno.ai/v1", Enabled: true,
		}}), "suno")
	})

	// Negatives: the authenticated opencode entry loads, and a
	// similar-but-unremoved host loads.
	t.Run("opencode authenticated entry loads", func(t *testing.T) {
		path := writeStoreFile(t, dir, []ProviderConfig{{
			ID: "prov-oc", Name: "OpenCode", CatalogID: "opencode", Enabled: true,
		}})
		reg, err := NewFileRegistry(path, WithCipher(cipher))
		if err != nil {
			t.Fatalf("load opencode = %v, want success (authenticated console entry is not the removed variant)", err)
		}
		p, ok := reg.Get("prov-oc")
		if !ok {
			t.Fatal("opencode provider not loaded")
		}
		if p.BaseURL != "https://opencode.ai/inference/openai/v1" {
			t.Fatalf("BaseURL = %q, want the catalog console base URL", p.BaseURL)
		}
	})
	t.Run("similar host loads", func(t *testing.T) {
		path := writeStoreFile(t, dir, []ProviderConfig{{
			ID: "prov-x", Name: "X", CatalogID: "groq",
			BaseURL: "https://notputer.com/v1", Enabled: true,
		}})
		if _, err := NewFileRegistry(path, WithCipher(cipher)); err != nil {
			t.Fatalf("load similar host = %v, want success", err)
		}
	})
}

func TestRegistryClientForCatalogExchangeError(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	_, dir := newCatalogTestRegistry(t)

	// A stored Vertex provider (base URL overridden to the mock) loads
	// fine, but building its client must fail cleanly: the credential
	// is a service-account JSON needing an exchange, never a static
	// header.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-vertex",
		Name:            "Vertex",
		BaseURL:         srv.URL,
		APIKeyEncrypted: sealTestKey(t, dir, "service-account-json"),
		CatalogID:       "vertex",
		AuthEndpoint:    "generate",
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	if _, err := reg.ClientFor("prov-vertex"); err == nil {
		t.Fatal("expected exchange error from ClientFor, got nil")
	} else if !strings.Contains(err.Error(), "exchange") {
		t.Errorf("error %q does not mention exchange", err.Error())
	}
}

func TestRegistryClientForAnthropicViaStore(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	_, dir := newCatalogTestRegistry(t)

	// A stored Anthropic provider (base URL overridden to the mock)
	// resolves Bearer + anthropic-version in the request path.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-anthropic",
		Name:            "Anthropic",
		BaseURL:         srv.URL,
		APIKeyEncrypted: sealTestKey(t, dir, "sk-ant-test"),
		CatalogID:       "anthropic",
		AuthEndpoint:    "messages",
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	client, err := reg.ClientFor("prov-anthropic")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/messages", "Authorization"); !ok || got != "Bearer sk-ant-test" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-ant-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/messages", "anthropic-version"); !ok || got != "2026-10-21" {
		t.Errorf("anthropic-version = %q (present=%v), want 2026-10-21", got, ok)
	}
}

func TestRegistryAddCatalogCohereNative(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// AddCatalog with --endpoint native: the native base, Cohere v2
	// wire, Bearer auth, verified-from-docs.
	p, err := reg.AddCatalogWithOptions(context.Background(), "cohere", "co-test-key",
		CatalogAddOptions{Endpoint: "native", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog cohere native: %v", err)
	}
	if p.CatalogID != "cohere" {
		t.Errorf("CatalogID = %q, want cohere", p.CatalogID)
	}
	if p.WireFormat != WireFormatCohere {
		t.Errorf("WireFormat = %q, want cohere", p.WireFormat)
	}
	if p.AuthEndpoint != "native" {
		t.Errorf("AuthEndpoint = %q, want native", p.AuthEndpoint)
	}
	if p.Verification != "verified-from-docs" {
		t.Errorf("Verification = %q, want verified-from-docs", p.Verification)
	}
	if len(p.Models) != 2 {
		t.Errorf("Models = %d, want 2", len(p.Models))
	}

	// The default endpoint (no --endpoint) is the OpenAI shim, not
	// the native surface: AddCatalog without an endpoint resolves
	// OpenAI wire on the compatibility base. (A second mock server:
	// the registry refuses a second provider on the same base URL.)
	srv2, _ := newRecordingServer(t)
	defer srv2.Close()
	p2, err := reg.AddCatalogWithOptions(context.Background(), "cohere", "co-test-key",
		CatalogAddOptions{BaseURL: srv2.URL})
	if err != nil {
		t.Fatalf("AddCatalog cohere default: %v", err)
	}
	if p2.WireFormat != WireFormatOpenAI {
		t.Errorf("default WireFormat = %q, want openai (compatibility shim)", p2.WireFormat)
	}
	if p2.AuthEndpoint != "chat" {
		t.Errorf("default AuthEndpoint = %q, want chat", p2.AuthEndpoint)
	}
}

func TestRegistryClientForCohereNative(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	_, dir := newCatalogTestRegistry(t)

	// A stored Cohere provider (base URL overridden to the mock)
	// resolves Bearer on the native surface and speaks /v2/chat.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-cohere",
		Name:            "Cohere",
		BaseURL:         srv.URL,
		APIKeyEncrypted: sealTestKey(t, dir, "co-test-key"),
		CatalogID:       "cohere",
		AuthEndpoint:    "native",
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	client, err := reg.ClientFor("prov-cohere")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "command-a-03-2025", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	if got, ok := mock.lastHeader("/v2/chat", "Authorization"); !ok || got != "Bearer co-test-key" {
		t.Errorf("Authorization = %q (present=%v), want Bearer co-test-key", got, ok)
	}
}

func TestRegistryClientForBaseURLFallback(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	_, dir := newCatalogTestRegistry(t)

	// A hand-added provider (no catalog link) whose base URL is a
	// catalog entry's Anthropic surface gets that surface's auth
	// (x-api-key) instead of the legacy Bearer default.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-ds",
		Name:            "DeepSeek",
		BaseURL:         "https://api.deepseek.com/anthropic",
		APIKeyEncrypted: sealTestKey(t, dir, "sk-ds-test"),
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	// The capturing factory routes the client at the mock (the stored
	// base URL is a real catalog URL) and records the headers the
	// registry resolved.
	var captured map[string]string
	factory := func(baseURL, apiKey string, authHeaders map[string]string, wireFormat string) (Client, error) {
		captured = authHeaders
		return NewClient(srv.URL, apiKey, WithAuthHeaders(authHeaders))
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)), WithClientFactory(factory))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	client, err := reg.ClientFor("prov-ds")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if captured == nil || captured["x-api-key"] != "sk-ds-test" {
		t.Fatalf("resolved headers = %v, want x-api-key sk-ds-test", captured)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "x-api-key"); !ok || got != "sk-ds-test" {
		t.Errorf("x-api-key = %q (present=%v), want sk-ds-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); ok {
		t.Errorf("Authorization must NOT be sent on the Anthropic surface, got %q", got)
	}
}

func TestRegistryClientForBaseURLFallbackExchange(t *testing.T) {
	_, dir := newCatalogTestRegistry(t)

	// A hand-added provider pointing at Bedrock's Converse surface
	// (SigV4 exchange) must fail at client build, never send a faked
	// Bearer header.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-bedrock",
		Name:            "Bedrock",
		BaseURL:         "https://bedrock-runtime.{region}.amazonaws.com",
		APIKeyEncrypted: sealTestKey(t, dir, "aws-key"),
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	if _, err := reg.ClientFor("prov-bedrock"); err == nil {
		t.Fatal("expected exchange error from ClientFor, got nil")
	} else if !strings.Contains(err.Error(), "exchange") {
		t.Errorf("error %q does not mention exchange", err.Error())
	}
}

func TestRegistryAddCustomKeepsLegacyBearer(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// A fully-custom provider (any base URL + key) still gets the
	// legacy Bearer default when the catalog does not know its URL.
	p, err := reg.Add(srv.URL, "sk-custom")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.CatalogID != "" || p.Verification != "" {
		t.Errorf("custom provider must have no catalog fields, got %+v", p)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); !ok || got != "Bearer sk-custom" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-custom", got, ok)
	}
}

func TestRegistryAddCatalogBedrockConverse(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	// AddCatalog bedrock with AWS credential material: the SigV4
	// exchange is performed by the client, so the add succeeds
	// (the loud refusal stays only for keys that are not AWS
	// credentials — see TestRegistryAddCatalogRefusals). The
	// default surface is converse (the first recorded surface:
	// bedrock's chat surface needs the "mantle" variant).
	p, err := reg.AddCatalogWithOptions(context.Background(), "bedrock", "AKIDEXAMPLE:SECRET",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog bedrock: %v", err)
	}
	if p.CatalogID != "bedrock" {
		t.Errorf("CatalogID = %q, want bedrock", p.CatalogID)
	}
	if p.WireFormat != WireFormatBedrock {
		t.Errorf("WireFormat = %q, want bedrock-converse", p.WireFormat)
	}
	if p.AuthEndpoint != "converse" {
		t.Errorf("AuthEndpoint = %q, want converse", p.AuthEndpoint)
	}
	if p.Verification != "verified-from-docs" {
		t.Errorf("Verification = %q, want verified-from-docs", p.Verification)
	}
	if len(p.Models) != 2 {
		t.Errorf("Models = %d, want 2", len(p.Models))
	}

	// ClientFor builds a Converse client: the chat hits
	// /model/{id}/converse and carries a SigV4 Authorization
	// header, never a Bearer header.
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, ok := client.(*bedrockClient); !ok {
		t.Fatalf("ClientFor returned %T, want *bedrockClient", client)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-3-haiku-20240307-v1:0", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	got, ok := mock.lastHeader("/model/anthropic.claude-3-haiku-20240307-v1:0/converse", "Authorization")
	if !ok || !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("Authorization = %q (present=%v), want SigV4 AKIDEXAMPLE credential", got, ok)
	}
	if got, ok := mock.lastHeader("/model/anthropic.claude-3-haiku-20240307-v1:0/converse", "X-Amz-Content-Sha256"); !ok || got == "" {
		t.Errorf("X-Amz-Content-Sha256 = %q (present=%v), want a sha256 hex", got, ok)
	}
}

func TestRegistryClientForBedrockNative(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	_, dir := newCatalogTestRegistry(t)

	// A stored Bedrock provider (base URL overridden to the mock)
	// resolves the SigV4 exchange and speaks /model/{id}/converse.
	path := writeStoreFile(t, dir, []ProviderConfig{{
		ID:              "prov-bedrock",
		Name:            "Bedrock",
		BaseURL:         srv.URL,
		APIKeyEncrypted: sealTestKey(t, dir, "AKIDEXAMPLE:SECRET"),
		CatalogID:       "bedrock",
		AuthEndpoint:    "converse",
		Enabled:         true,
	}})
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	reg, err := NewFileRegistry(path, WithCipher(NewVaultCipher(vault)))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	client, err := reg.ClientFor("prov-bedrock")
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, ok := client.(*bedrockClient); !ok {
		t.Fatalf("ClientFor returned %T, want *bedrockClient", client)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{Model: "anthropic.claude-3-haiku-20240307-v1:0", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi" {
		t.Errorf("Content = %q, want hi", resp.Content)
	}
	got, ok := mock.lastHeader("/model/anthropic.claude-3-haiku-20240307-v1:0/converse", "Authorization")
	if !ok || !strings.HasPrefix(got, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") {
		t.Errorf("Authorization = %q (present=%v), want SigV4 AKIDEXAMPLE credential", got, ok)
	}
}
