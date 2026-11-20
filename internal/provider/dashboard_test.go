package provider

import (
	"context"
	"testing"
)

// TestRegistryDashboardProviders proves the dashboard summary comes
// from the live registry: verification status, wire format and auth
// kind per provider, with no secrets anywhere in the output.
func TestRegistryDashboardProviders(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	srv2, _ := newRecordingServer(t)
	defer srv2.Close()
	srv3, _ := newRecordingServer(t)
	defer srv3.Close()
	reg, _ := newCatalogTestRegistry(t)
	ctx := context.Background()

	// A catalog provider verified from its own docs (Groq: Bearer on
	// its chat surface, OpenAI wire).
	groq, err := reg.AddCatalogWithOptions(ctx, "groq", "sk-groq-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog groq: %v", err)
	}
	// A catalog provider on a non-default surface: DeepSeek's
	// Anthropic surface sends x-api-key, not Bearer.
	deepseek, err := reg.AddCatalogWithOptions(ctx, "deepseek", "sk-deepseek-test",
		CatalogAddOptions{BaseURL: srv2.URL, Endpoint: "anthropic"})
	if err != nil {
		t.Fatalf("AddCatalog deepseek anthropic: %v", err)
	}
	// A hand-added custom provider: unverified, legacy Bearer.
	custom, err := reg.AddContext(ctx, "My Custom", srv3.URL, "sk-custom-test")
	if err != nil {
		t.Fatalf("Add custom: %v", err)
	}
	if err := reg.SetEnabled(custom.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	got := reg.DashboardProviders()
	if len(got) != 3 {
		t.Fatalf("DashboardProviders = %d providers, want 3", len(got))
	}
	byID := map[string]DashboardProvider{}
	for _, d := range got {
		byID[d.ID] = d
	}

	g := byID[groq.ID]
	if g.Name != "Groq" || g.CatalogID != "groq" {
		t.Errorf("groq identity = %+v", g)
	}
	if g.Verification != "verified-from-docs" {
		t.Errorf("groq verification = %q, want verified-from-docs", g.Verification)
	}
	if g.WireFormat != WireFormatOpenAI {
		t.Errorf("groq wire = %q, want openai", g.WireFormat)
	}
	if g.AuthKind != "bearer" {
		t.Errorf("groq auth_kind = %q, want bearer", g.AuthKind)
	}
	if g.AuthEndpoint != "chat" || !g.Enabled || g.Models != 2 {
		t.Errorf("groq = %+v, want chat endpoint, enabled, 2 models", g)
	}

	d := byID[deepseek.ID]
	if d.Verification != "verified-from-docs" {
		t.Errorf("deepseek verification = %q, want verified-from-docs", d.Verification)
	}
	if d.AuthKind != "x-api-key" {
		t.Errorf("deepseek anthropic-surface auth_kind = %q, want x-api-key", d.AuthKind)
	}
	if d.AuthEndpoint != "anthropic" {
		t.Errorf("deepseek auth_endpoint = %q, want anthropic", d.AuthEndpoint)
	}

	c := byID[custom.ID]
	if c.Verification != "unverified" {
		t.Errorf("custom verification = %q, want unverified", c.Verification)
	}
	if c.AuthKind != "bearer" {
		t.Errorf("custom auth_kind = %q, want bearer (legacy default)", c.AuthKind)
	}
	if c.CatalogID != "" {
		t.Errorf("custom catalog_id = %q, want empty", c.CatalogID)
	}
	if c.Enabled {
		t.Errorf("custom enabled = true, want false (disabled above)")
	}
	if c.WireFormat != WireFormatOpenAI {
		t.Errorf("custom wire = %q, want openai", c.WireFormat)
	}
}

// TestRegistryDashboardProvidersEmpty proves an empty registry yields
// an empty (non-nil) list, so the dashboard renders "no providers"
// instead of crashing on a null.
func TestRegistryDashboardProvidersEmpty(t *testing.T) {
	reg, _ := newCatalogTestRegistry(t)
	got := reg.DashboardProviders()
	if got == nil {
		t.Fatal("DashboardProviders = nil, want empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("DashboardProviders = %d providers, want 0", len(got))
	}
}

// TestRegistryDashboardProvidersKeyCreationURL proves the dashboard
// summary carries the catalog entry's key-creation page
// (parity tracker Area 3, "Key-creation links"): a catalog provider whose
// entry records one exposes it, a custom provider reports none.
// The URL is a public key page, never a credential or base URL.
func TestRegistryDashboardProvidersKeyCreationURL(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	srv2, _ := newRecordingServer(t)
	defer srv2.Close()
	reg, _ := newCatalogTestRegistry(t)
	ctx := context.Background()

	// BeatAPI's catalog entry is seeded with a key page
	// (internal/catalog/keycreation.go).
	beat, err := reg.AddCatalogWithOptions(ctx, "beatapi", "sk-beatapi-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog beatapi: %v", err)
	}
	custom, err := reg.AddContext(ctx, "My Custom", srv2.URL, "sk-custom-test")
	if err != nil {
		t.Fatalf("Add custom: %v", err)
	}

	got := reg.DashboardProviders()
	if len(got) != 2 {
		t.Fatalf("DashboardProviders = %d providers, want 2", len(got))
	}
	byID := map[string]DashboardProvider{}
	for _, d := range got {
		byID[d.ID] = d
	}

	b := byID[beat.ID]
	if b.CatalogID != "beatapi" {
		t.Errorf("beatapi catalog_id = %q, want beatapi", b.CatalogID)
	}
	if b.KeyCreationURL != "https://beatapi.io/dashboard/apikeys" {
		t.Errorf("beatapi key_creation_url = %q, want https://beatapi.io/dashboard/apikeys", b.KeyCreationURL)
	}

	c := byID[custom.ID]
	if c.KeyCreationURL != "" {
		t.Errorf("custom key_creation_url = %q, want empty", c.KeyCreationURL)
	}
}
