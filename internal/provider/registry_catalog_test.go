package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// recordingUpstream serves /v1/models and /v1/chat/completions and
// records every request's path and headers, so tests can prove which
// auth the client actually sent.
type recordingUpstream struct {
	mu       sync.Mutex
	models   string
	chatBody string
	seen     []recordedRequest
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
		default:
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
		models:   `{"data":[{"id":"model-a"},{"id":"model-b"}]}`,
		chatBody: `{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
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
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); !ok || got != "Bearer sk-ant-test" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-ant-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "anthropic-version"); !ok || got != "2026-10-21" {
		t.Errorf("anthropic-version = %q (present=%v), want 2026-10-21", got, ok)
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
	if got, ok := mock.lastHeader("/v1/chat/completions", "Authorization"); !ok || got != "Bearer sk-ant-test" {
		t.Errorf("Authorization = %q (present=%v), want Bearer sk-ant-test", got, ok)
	}
	if got, ok := mock.lastHeader("/v1/chat/completions", "anthropic-version"); !ok || got != "2026-10-21" {
		t.Errorf("anthropic-version = %q (present=%v), want 2026-10-21", got, ok)
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
	factory := func(baseURL, apiKey string, authHeaders map[string]string) (Client, error) {
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
