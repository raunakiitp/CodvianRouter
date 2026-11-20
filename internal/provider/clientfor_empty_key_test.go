// Regression tests for CHECKLIST 313: ClientFor must serve a local
// no-auth provider (e.g. Ollama) whose stored key is empty.
//
// Root cause: a provider stored with no key at all carries an empty
// ciphertext (APIKeyEncrypted == ""), and clientForKey fed it to
// decryptKey unconditionally. Vault.Unseal rejects "" as an unknown
// seal format (no "v1:"/"v2:" prefix), so ClientFor failed for
// exactly the providers that need no key — while RelayTargetForModel
// had already been fixed to skip decryption when no key is stored.
// The downstream path was always fine: catalog.ResolveAuth returns
// empty headers and no error for AuthNone surfaces with an empty
// credential, and NewClient accepts an empty key.
package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ollamaWireMock is a minimal Ollama-wire upstream: the OpenAI-style
// /v1/models + /v1/chat/completions Ollama also serves, plus the
// /api/tags probe the readiness gate uses. It records whether any
// request carried an Authorization header — a no-auth upstream must
// never receive one, not even an empty Bearer.
type ollamaWireMock struct {
	mu        sync.Mutex
	chatHits  int
	sawAuth   bool
	lastModel string
}

func (m *ollamaWireMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	if h := r.Header.Get("Authorization"); h != "" {
		m.sawAuth = true
	}
	m.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/tags":
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2","model":"llama3.2"}]}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"llama3.2","object":"model"}]}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/chat/completions":
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		m.mu.Lock()
		m.chatHits++
		m.lastModel = req.Model
		m.mu.Unlock()
		_, _ = w.Write([]byte(`{"id":"chatcmpl-smoke","object":"chat.completion","created":1,"model":"llama3.2","choices":[{"index":0,"message":{"role":"assistant","content":"smoke says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (m *ollamaWireMock) stats() (chatHits int, sawAuth bool, lastModel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chatHits, m.sawAuth, m.lastModel
}

func TestClientForEmptyKeyLocalNoAuthOllama(t *testing.T) {
	mock := &ollamaWireMock{}
	srv := httptest.NewServer(mock)
	defer srv.Close()

	// Stored exactly as a no-auth provider with no key at all:
	// APIKeyEncrypted is the empty string, not a seal of "".
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "ollama-local", Name: "Ollama", BaseURL: srv.URL, APIKeyEncrypted: "", WireFormat: "ollama", Models: []Model{{ID: "llama3.2"}}, Enabled: true, CatalogID: "ollama-local"},
	})

	client, err := reg.ClientFor("ollama-local")
	if err != nil {
		t.Fatalf("ClientFor(empty-key Ollama): %v", err)
	}
	if client == nil {
		t.Fatal("ClientFor returned a nil client with no error")
	}
	if err := client.TestKey(context.Background()); err != nil {
		t.Fatalf("TestKey: %v", err)
	}
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    "llama3.2",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "smoke says hi" {
		t.Errorf("Content = %q, want %q", resp.Content, "smoke says hi")
	}
	hits, sawAuth, lastModel := mock.stats()
	if hits != 1 {
		t.Errorf("chat hits = %d, want 1", hits)
	}
	if lastModel != "llama3.2" {
		t.Errorf("upstream model = %q, want llama3.2", lastModel)
	}
	if sawAuth {
		t.Error("upstream saw an Authorization header; a no-auth provider must send none")
	}
}

func TestDecryptedKeyEmptyForNoAuthProvider(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "ollama-local", Name: "Ollama", BaseURL: "http://127.0.0.1:11434", APIKeyEncrypted: "", WireFormat: "ollama", Models: []Model{{ID: "llama3.2"}}, Enabled: true, CatalogID: "ollama-local"},
	})
	key, err := reg.DecryptedKey("ollama-local")
	if err != nil {
		t.Fatalf("DecryptedKey: %v", err)
	}
	if key != "" {
		t.Errorf("DecryptedKey = %q, want empty", key)
	}
}

func TestClientForEmptyKeyAuthProviderStillFails(t *testing.T) {
	// The fix must not turn an empty key into silent no-auth for a
	// provider whose surface requires a credential: a catalog auth
	// provider stored with no key still fails honestly at
	// ClientFor, naming the missing credential.
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "oai", Name: "OpenAI", BaseURL: "https://api.openai.com", APIKeyEncrypted: "", WireFormat: WireFormatOpenAI, Models: []Model{{ID: "gpt-4o-mini"}}, Enabled: true, CatalogID: "openai"},
	})
	_, err := reg.ClientFor("oai")
	if err == nil {
		t.Fatal("ClientFor(empty-key OpenAI) succeeded; want an error naming the missing credential")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error = %q, want it to name the missing credential", err.Error())
	}
}
