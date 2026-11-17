package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// mockUpstream serves /v1/models and /v1/chat/completions with configurable
// statuses, for registry Add / wire-format detection tests.
type mockUpstream struct {
	modelsStatus int
	modelsBody   string
	chatStatus   int
	chatBody     string
}

func (m *mockUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(m.modelsStatus)
			_, _ = w.Write([]byte(m.modelsBody))
		case "/v1/chat/completions":
			w.WriteHeader(m.chatStatus)
			_, _ = w.Write([]byte(m.chatBody))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func openAIMock(t *testing.T) *httptest.Server {
	t.Helper()
	m := &mockUpstream{
		modelsStatus: 200,
		modelsBody:   `{"data":[{"id":"model-a"},{"id":"model-b"}]}`,
		chatStatus:   200,
		chatBody:     `{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`,
	}
	return httptest.NewServer(m.handler(t))
}

func TestRegistryAddSuccess(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	dir := t.TempDir()
	// The test reloads the store through a second Registry instance, so
	// both instances share one Vault-backed cipher (a fixed test secret);
	// the ephemeral default cipher would not survive the reload.
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)
	reg, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(cipher))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	p, err := reg.Add(srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.ID == "" || p.Name == "" {
		t.Errorf("provider missing ID/Name: %+v", p)
	}
	if p.BaseURL != srv.URL {
		t.Errorf("BaseURL = %q, want %q", p.BaseURL, srv.URL)
	}
	if !p.Enabled {
		t.Error("new provider should be enabled")
	}
	if p.WireFormat != WireFormatOpenAI {
		t.Errorf("WireFormat = %q", p.WireFormat)
	}
	if p.QuotaHint != QuotaUnknown {
		t.Errorf("QuotaHint = %v, want -1", p.QuotaHint)
	}
	if len(p.Models) != 2 || p.Models[0].ID != "model-a" || p.Models[1].ID != "model-b" {
		t.Errorf("Models = %+v", p.Models)
	}
	if p.APIKeyEncrypted == "" {
		t.Error("APIKeyEncrypted empty")
	}

	// Persisted: a fresh registry on the same file reloads the provider.
	reg2, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(cipher))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got, ok := reg2.Get(p.ID)
	if !ok {
		t.Fatal("provider not persisted")
	}
	if got.BaseURL != p.BaseURL || len(got.Models) != 2 {
		t.Errorf("reloaded provider = %+v", got)
	}
	if key, err := reg2.DecryptedKey(p.ID); err != nil || key != "sk-test" {
		t.Errorf("DecryptedKey = %q, err = %v", key, err)
	}
	if _, err := reg2.ClientFor(p.ID); err != nil {
		t.Errorf("ClientFor: %v", err)
	}
}

func TestRegistryAddInvalidURL(t *testing.T) {
	reg := NewRegistry(nil)
	for _, bad := range []string{"", "://bad", "ftp://example.com", "example.com", "http://"} {
		if _, err := reg.Add(bad, "key"); err == nil {
			t.Errorf("Add(%q) should fail", bad)
		}
	}
	if _, err := reg.Add("https://example.com", ""); err == nil {
		t.Error("empty API key should fail")
	}
	if n := len(reg.List()); n != 0 {
		t.Errorf("failed adds left %d providers", n)
	}
}

func TestRegistryAddBadKey(t *testing.T) {
	m := &mockUpstream{
		modelsStatus: 401,
		modelsBody:   `{"error":"bad key"}`,
		chatStatus:   401,
	}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()

	reg := NewRegistry(nil)
	if _, err := reg.Add(srv.URL, "sk-wrong"); err == nil {
		t.Fatal("Add with bad key should fail")
	}
	if n := len(reg.List()); n != 0 {
		t.Errorf("failed add left %d providers", n)
	}
}

func TestRegistryAddDuplicate(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	reg := NewRegistry(nil)
	if _, err := reg.Add(srv.URL, "k1"); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	if _, err := reg.Add(srv.URL+"/", "k2"); err == nil {
		t.Fatal("duplicate base URL (trailing slash variant) should fail")
	} else if !errors.Is(err, ErrProviderExists) {
		t.Errorf("error = %v, want ErrProviderExists", err)
	}
}

func TestRegistryAddModels404ChatExists(t *testing.T) {
	// Gateway with no /v1/models but a working chat endpoint: still added,
	// with an empty model list.
	m := &mockUpstream{
		modelsStatus: 404,
		modelsBody:   `{"error":"not found"}`,
		chatStatus:   400,
		chatBody:     `{"error":{"message":"model __codvian_router_probe__ not found"}}`,
	}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()

	reg := NewRegistry(nil)
	p, err := reg.Add(srv.URL, "k")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if p.WireFormat != WireFormatOpenAI {
		t.Errorf("WireFormat = %q, want openai", p.WireFormat)
	}
	if len(p.Models) != 0 {
		t.Errorf("Models = %+v, want empty", p.Models)
	}
}

func TestRegistryAddModels404Chat404(t *testing.T) {
	m := &mockUpstream{
		modelsStatus: 404,
		modelsBody:   `{"error":"not found"}`,
		chatStatus:   404,
		chatBody:     `{"error":"route not found"}`,
	}
	srv := httptest.NewServer(m.handler(t))
	defer srv.Close()

	reg := NewRegistry(nil)
	if _, err := reg.Add(srv.URL, "k"); err == nil {
		t.Fatal("Add should fail when neither endpoint exists")
	}
}

func TestRegistryRemoveGetList(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	reg := NewRegistry(nil)
	p1, err := reg.Add(srv.URL, "k1")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got, ok := reg.Get(p1.ID); !ok || got.BaseURL != p1.BaseURL {
		t.Errorf("Get = %+v, %v", got, ok)
	}
	if _, ok := reg.Get("nope"); ok {
		t.Error("Get of unknown ID should be false")
	}
	if err := reg.SetEnabled(p1.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if got, _ := reg.Get(p1.ID); got.Enabled {
		t.Error("SetEnabled(false) did not stick")
	}
	if err := reg.SetQuotaHint(p1.ID, 0.5); err != nil {
		t.Fatalf("SetQuotaHint: %v", err)
	}
	if got, _ := reg.Get(p1.ID); got.QuotaHint != 0.5 {
		t.Errorf("QuotaHint = %v", got.QuotaHint)
	}
	if err := reg.SetQuotaHint(p1.ID, 1.5); err == nil {
		t.Error("quota hint > 1 should fail")
	}
	if err := reg.Remove("nope"); !errors.Is(err, ErrProviderNotFound) {
		t.Errorf("Remove unknown: %v", err)
	}
	if err := reg.Remove(p1.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(reg.List()) != 0 {
		t.Error("registry should be empty after Remove")
	}
	// Get must return a copy: mutating it must not affect the registry.
	p2, _ := reg.Add(srv.URL, "k2")
	got, _ := reg.Get(p2.ID)
	got.Models = append(got.Models, Model{ID: "fake"})
	if again, _ := reg.Get(p2.ID); len(again.Models) != 2 {
		t.Errorf("Get returned a live reference, models = %v", again.Models)
	}
}

func TestDetectWireFormat(t *testing.T) {
	ctx := context.Background()
	factory := func(baseURL, apiKey string) (Client, error) {
		return NewClient(baseURL, apiKey)
	}

	// Models OK -> openai.
	srv := openAIMock(t)
	defer srv.Close()
	if got, err := DetectWireFormat(ctx, srv.URL, "k", factory); err != nil || got != WireFormatOpenAI {
		t.Errorf("models-ok: got %q, err %v", got, err)
	}

	// Models 404, chat probe answers 400 mentioning the model -> openai.
	m := &mockUpstream{modelsStatus: 404, modelsBody: `{}`, chatStatus: 400,
		chatBody: `{"error":{"message":"model not found"}}`}
	srv2 := httptest.NewServer(m.handler(t))
	defer srv2.Close()
	if got, err := DetectWireFormat(ctx, srv2.URL, "k", factory); err != nil || got != WireFormatOpenAI {
		t.Errorf("chat-exists: got %q, err %v", got, err)
	}

	// Both 404 -> unknown.
	m2 := &mockUpstream{modelsStatus: 404, modelsBody: `{}`, chatStatus: 404,
		chatBody: `{"error":"no such route"}`}
	srv3 := httptest.NewServer(m2.handler(t))
	defer srv3.Close()
	if got, err := DetectWireFormat(ctx, srv3.URL, "k", factory); err != nil || got != WireFormatUnknown {
		t.Errorf("both-404: got %q, err %v", got, err)
	}
}

func TestCodvianPreset(t *testing.T) {
	if CodvianPreset.Name != "Codvian" {
		t.Errorf("preset name = %q", CodvianPreset.Name)
	}
	if CodvianPreset.BaseURL != "https://api.codvian.com/v1" {
		t.Errorf("preset base URL = %q", CodvianPreset.BaseURL)
	}
	if _, err := ValidateBaseURL(CodvianPreset.BaseURL); err != nil {
		t.Errorf("preset base URL invalid: %v", err)
	}
}

func TestDeriveName(t *testing.T) {
	if got := DeriveName("https://api.codvian.com/v1"); got != "Codvian" {
		t.Errorf("DeriveName codvian = %q", got)
	}
	if got := DeriveName("https://api.openai.com"); got != "Openai" {
		t.Errorf("DeriveName openai = %q", got)
	}
}

func TestProviderConfigRoundTrip(t *testing.T) {
	p := &Provider{
		ID: "prov_x", Name: "X", BaseURL: "https://x.example.com/v1",
		APIKeyEncrypted: "enc", WireFormat: WireFormatOpenAI,
		Models:    []Model{{ID: "m", Name: "m", ContextWindow: 128000}},
		Enabled:   true,
		QuotaHint: 0.25,
	}
	back := ProviderFromConfig(p.ToConfig())
	if back.ID != p.ID || back.Name != p.Name || back.BaseURL != p.BaseURL ||
		back.APIKeyEncrypted != p.APIKeyEncrypted || back.WireFormat != p.WireFormat ||
		back.QuotaHint != p.QuotaHint || len(back.Models) != 1 || back.Models[0].ContextWindow != 128000 {
		t.Errorf("round trip mismatch: %+v", back)
	}
	// Missing wire format defaults to openai.
	if got := ProviderFromConfig(ProviderConfig{ID: "a"}); got.WireFormat != WireFormatOpenAI {
		t.Errorf("default wire format = %q", got.WireFormat)
	}
}

func TestJSONFileStoreMissingFile(t *testing.T) {
	s := NewJSONFileStore(filepath.Join(t.TempDir(), "nope.json"))
	configs, err := s.Load()
	if err != nil || configs != nil {
		t.Errorf("missing file: configs=%v err=%v", configs, err)
	}
}

func TestJSONFileStoreRoundTripAndPerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "providers.json")
	s := NewJSONFileStore(path)
	cfgs := []ProviderConfig{{ID: "prov_1", Name: "A", BaseURL: "https://a.example.com", Enabled: true, QuotaHint: -1}}
	if err := s.Save(cfgs); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("store perms = %o, want 0600", info.Mode().Perm())
	}
	loaded, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != "prov_1" {
		t.Errorf("loaded = %+v", loaded)
	}
}
