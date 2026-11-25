package provider

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// memRelayStore is an in-memory Store preloaded with providers, so
// RelayTarget tests do not need network access or a file.
type memRelayStore struct {
	providers []ProviderConfig
}

func (m *memRelayStore) Load() ([]ProviderConfig, error) { return m.providers, nil }
func (m *memRelayStore) Save(ps []ProviderConfig) error  { m.providers = ps; return nil }

func testRelayRegistry(t *testing.T, cfgs []ProviderConfig) *Registry {
	t.Helper()
	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-relay!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)
	store := &memRelayStore{}
	reg := NewRegistry(store, WithCipher(cipher), WithRegistryAllowPrivateDestinations(true))
	// Seal each key through the registry's cipher, then reload from
	// the store so the registry under test holds the sealed form.
	sealed := make([]ProviderConfig, len(cfgs))
	for i, c := range cfgs {
		sealed[i] = c
		if c.APIKeyEncrypted != "" {
			enc, err := cipher.Encrypt(c.APIKeyEncrypted)
			if err != nil {
				t.Fatalf("seal key: %v", err)
			}
			sealed[i].APIKeyEncrypted = enc
		}
	}
	if err := store.Save(sealed); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	reg2 := NewRegistry(store, WithCipher(cipher), WithRegistryAllowPrivateDestinations(true))
	if err := reg2.Load(); err != nil {
		t.Fatalf("load seeded registry: %v", err)
	}
	if got := reg2.List(); len(got) != len(cfgs) {
		t.Fatalf("seeded registry lists %d providers, want %d", len(got), len(cfgs))
	}
	_ = reg
	return reg2
}

func TestRelayTargetSingleAnthropicProvider(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "anth", Name: "Anthropic", BaseURL: "https://api.anthropic.com", APIKeyEncrypted: "sk-ant-test", WireFormat: WireFormatAnthropic, Models: []Model{{ID: "claude-sonnet-4-5"}}, Enabled: true, CatalogID: "anthropic"},
	})
	tgt, err := reg.RelayTargetForModel("claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("RelayTargetForModel: %v", err)
	}
	if tgt.WireFormat != WireFormatAnthropic {
		t.Errorf("WireFormat = %q, want anthropic", tgt.WireFormat)
	}
	if tgt.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", tgt.Attempts)
	}
	if tgt.IsLocal {
		t.Error("Anthropic target must not be local")
	}
	if len(tgt.AuthHeaders) == 0 {
		t.Error("auth headers must be resolved from the catalog, never empty for Anthropic")
	}
	if tgt.UpstreamModel != "claude-sonnet-4-5" {
		t.Errorf("UpstreamModel = %q", tgt.UpstreamModel)
	}
}

func TestRelayTargetMultiAttemptCountsProviders(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "a", Name: "A", BaseURL: "https://a.example", APIKeyEncrypted: "k1", WireFormat: WireFormatOpenAI, Models: []Model{{ID: "shared-model"}}, Enabled: true},
		{ID: "b", Name: "B", BaseURL: "https://b.example", APIKeyEncrypted: "k2", WireFormat: WireFormatOpenAI, Models: []Model{{ID: "shared-model"}}, Enabled: true},
		{ID: "off", Name: "Off", BaseURL: "https://off.example", APIKeyEncrypted: "k3", WireFormat: WireFormatOpenAI, Models: []Model{{ID: "shared-model"}}, Enabled: false},
	})
	tgt, err := reg.RelayTargetForModel("shared-model")
	if err != nil {
		t.Fatalf("RelayTargetForModel: %v", err)
	}
	// Two enabled providers serve it; the disabled one is not an
	// attempt, so the relay must translate (failover needs the
	// canonical form) instead of forwarding verbatim.
	if tgt.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (disabled provider excluded)", tgt.Attempts)
	}
	if tgt.ProviderID != "a" {
		t.Errorf("ProviderID = %q, want first provider a", tgt.ProviderID)
	}
	if _, err := reg.RelayTargetForModel("no-such-model"); err == nil {
		t.Error("unknown model should error, not invent a target")
	}
	_ = context.Background
}

func TestRelayTargetLocalOllama(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "ollama-local", Name: "Ollama", BaseURL: "http://127.0.0.1:11434", APIKeyEncrypted: "", WireFormat: "ollama", Models: []Model{{ID: "llama3.2"}}, Enabled: true, CatalogID: "ollama-local"},
	})
	tgt, err := reg.RelayTargetForModel("llama3.2")
	if err != nil {
		t.Fatalf("RelayTargetForModel: %v", err)
	}
	if !tgt.IsLocal {
		t.Error("ollama-local target must be local (readiness-gated)")
	}
}

func TestDashboardModelsMarksDeprecated(t *testing.T) {
	reg := testRelayRegistry(t, []ProviderConfig{
		{ID: "oai", Name: "OpenAI", BaseURL: "https://api.openai.com", APIKeyEncrypted: "sk-test", WireFormat: WireFormatOpenAI, Enabled: true, Models: []Model{
			{ID: "gpt-4-0314"},               // catalog-retired, no flag set
			{ID: "gpt-4o", Deprecated: true}, // operator-flagged
			{ID: "gpt-4o-mini"},              // current
		}},
	})
	got := reg.DashboardModels()
	byID := map[string]DashboardModel{}
	for _, m := range got {
		byID[m.ID] = m
	}
	if !byID["gpt-4-0314"].Deprecated {
		t.Error("gpt-4-0314 should be deprecated via the catalog table even without the flag")
	}
	if !byID["gpt-4o"].Deprecated {
		t.Error("operator-flagged gpt-4o should be deprecated")
	}
	if byID["gpt-4o-mini"].Deprecated {
		t.Error("gpt-4o-mini must not be deprecated")
	}
	if len(got) != 3 {
		t.Errorf("DashboardModels = %d rows, want 3 (deprecated rows are marked, not dropped here)", len(got))
	}
}
