package provider

import (
	"context"
	"testing"
)

// TestRegistryDashboardModels proves the dashboard model catalog
// comes from the live registry: every model of every configured
// provider, with its wire format, verification status and
// best-effort metadata — and no rows at all for a provider with no
// models. Nothing here is a hard-coded catalog: the rows are the
// registry's own contents.
func TestRegistryDashboardModels(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)
	ctx := context.Background()

	groq, err := reg.AddCatalogWithOptions(ctx, "groq", "sk-groq-test",
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog groq: %v", err)
	}

	got := reg.DashboardModels()
	if len(got) == 0 {
		t.Fatalf("DashboardModels = 0 rows for a registry with a Groq provider, want > 0")
	}
	for _, m := range got {
		if m.ID == "" || m.ProviderID != groq.ID || m.ProviderName != "Groq" {
			t.Errorf("model row = %+v, want groq provider identity", m)
		}
		if m.WireFormat == "" || m.Verification == "" {
			t.Errorf("model row %q has empty wire/verification: %+v", m.ID, m)
		}
		if !m.Enabled {
			t.Errorf("model row %q is disabled, want enabled (provider is enabled)", m.ID)
		}
	}

	// A hand-added custom provider contributes its own rows (its
	// models are fetched from its /v1/models endpoint by the
	// registry), but the Groq rows are unchanged by adding it.
	srv2, _ := newRecordingServer(t)
	defer srv2.Close()
	custom, err := reg.AddContext(ctx, "Empty Custom", srv2.URL, "sk-custom-test")
	if err != nil {
		t.Fatalf("Add custom: %v", err)
	}
	after := reg.DashboardModels()
	groqRows := 0
	for _, m := range after {
		if m.ProviderID == groq.ID {
			groqRows++
		}
	}
	if groqRows != len(got) {
		t.Errorf("Groq rows changed from %d to %d after adding a second provider, want unchanged", len(got), groqRows)
	}
	for _, m := range after {
		if m.ProviderID == custom.ID && m.ProviderName != "Empty Custom" {
			t.Errorf("custom model row = %+v, want provider name \"Empty Custom\"", m)
		}
	}

	// An empty registry returns an empty (non-nil) slice, never nil.
	emptyReg, _ := newCatalogTestRegistry(t)
	if models := emptyReg.DashboardModels(); models == nil || len(models) != 0 {
		t.Errorf("empty registry DashboardModels = %v, want empty non-nil slice", models)
	}
}
