// Removed-provider enforcement tests (G19 Area 1, CT-2).
package provider

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

func TestG19IsRemovedVariant(t *testing.T) {
	// Whole-integration removal matches any entry with the ID.
	p := &Provider{ID: "puter", CatalogID: "puter"}
	if _, ok := p.IsRemovedVariant(); !ok {
		t.Fatal("puter provider not reported as removed variant")
	}
	// Alias matches too.
	p = &Provider{ID: "pu", CatalogID: "pu"}
	if _, ok := p.IsRemovedVariant(); !ok {
		t.Fatal("puter alias pu not reported as removed variant")
	}
	// Keyless-only removal: a keyless opencode entry matches...
	p = &Provider{ID: "opencode", CatalogID: "opencode"}
	if _, ok := p.IsRemovedVariant(); !ok {
		t.Fatal("keyless opencode not reported as removed variant")
	}
	// ...but the authenticated console API does not.
	p = &Provider{ID: "opencode", CatalogID: "opencode", APIKeyEncrypted: "sealed"}
	if _, ok := p.IsRemovedVariant(); ok {
		t.Fatal("authenticated opencode reported as removed variant: only the keyless surface was removed")
	}
	p = &Provider{ID: "opencode", CatalogID: "opencode", Keys: []ProviderKey{{ID: "k1"}}}
	if _, ok := p.IsRemovedVariant(); ok {
		t.Fatal("opencode with stored keys reported as removed variant")
	}
	// An ordinary provider never matches.
	p = &Provider{ID: "openai", CatalogID: "openai"}
	if _, ok := p.IsRemovedVariant(); ok {
		t.Fatal("openai reported as removed variant")
	}
	if _, ok := (*Provider)(nil).IsRemovedVariant(); ok {
		t.Fatal("nil provider reported as removed variant")
	}
}

func TestG19IsRemovedBaseURL(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://puter.com/v1", true},
		{"https://api.puter.com/v1/chat", true}, // sub-domain
		{"https://notputer.com/v1", false},
		{"https://business.gemini.google/v1", true},
		{"https://studio-api.suno.ai/v1", true},
		{"https://kie.ai/v1", false},                // kie-hosted suno is unaffected
		{"https://opencode.ai/zen/v1", true},        // the removed keyless surface
		{"https://opencode.ai/inference/v1", false}, // authenticated console API
		{"https://example.com/v1", false},
		{"not-a-url", false},
		{"", false},
	}
	for _, tc := range cases {
		_, got := IsRemovedBaseURL(tc.url)
		if got != tc.want {
			t.Fatalf("IsRemovedBaseURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestG19AddRejectsRemovedBaseURL(t *testing.T) {
	// The domain check runs before any network call (TestKey),
	// so a removed base URL fails fast with the removal error.
	// Every removed domain is covered, including sub-domains.
	reg := NewRegistry(nil)
	for _, tc := range []struct {
		name string
		url  string
		want string // removed provider ID the error must name
	}{
		{"puter", "https://puter.com/v1", "puter"},
		{"puter subdomain", "https://api.puter.com/v1/chat", "puter"},
		{"theoldllm", "https://theoldllm.com/v1", "theoldllm"},
		{"theoldllm vercel", "https://theoldllm.vercel.app/v1", "theoldllm"},
		{"gemini-business", "https://business.gemini.google/v1", "gemini-business"},
		{"suno", "https://studio-api.suno.ai/v1", "suno"},
		{"suno prod", "https://studio-api-prod.suno.com/v1", "suno"},
		{"opencode zen", "https://opencode.ai/zen/v1", "opencode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := reg.AddContext(context.Background(), "x", tc.url, "test-key")
			if err == nil {
				t.Fatalf("Add(%s) succeeded, want a removed-provider error", tc.url)
			}
			var remErr *catalog.RemovedProviderError
			if !errors.As(err, &remErr) {
				t.Fatalf("Add(%s) error = %v, want typed *catalog.RemovedProviderError", tc.url, err)
			}
			if remErr.Removed.ID != tc.want {
				t.Fatalf("Add(%s) removal names %q, want %q", tc.url, remErr.Removed.ID, tc.want)
			}
			if !strings.Contains(err.Error(), "was removed on") || !strings.Contains(err.Error(), remErr.Removed.RemovalRef) {
				t.Fatalf("Add(%s) error = %v, want it to name the removal (date + ref)", tc.url, err)
			}
		})
	}
}

// TestCT2AddCatalogRefusesRemovedIDs — CT-2 acceptance (1): adding a
// removed catalog ID via AddCatalogWithOptions is refused with a
// typed error naming the removal. Each of the 5 register entries is
// covered: the four whole-integration removals by ID and alias, and
// opencode by the scope exception — its catalog entry is the
// legitimate authenticated console API, so the add must NOT be
// refused as a removal (it proceeds to the key test against the
// mock upstream).
func TestCT2AddCatalogRefusesRemovedIDs(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	for _, tc := range []struct {
		id   string
		want string
	}{
		{"puter", "puter"},
		{"pu", "puter"}, // alias
		{"theoldllm", "theoldllm"},
		{"tllm", "theoldllm"}, // alias
		{"gemini-business", "gemini-business"},
		{"gembiz", "gemini-business"}, // alias
		{"suno", "suno"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			_, err := reg.AddCatalogWithOptions(context.Background(), tc.id, "k",
				CatalogAddOptions{BaseURL: srv.URL})
			if err == nil {
				t.Fatalf("AddCatalog(%q) succeeded, want a removed-provider error", tc.id)
			}
			var remErr *catalog.RemovedProviderError
			if !errors.As(err, &remErr) {
				t.Fatalf("AddCatalog(%q) error = %v, want typed *catalog.RemovedProviderError", tc.id, err)
			}
			if remErr.Removed.ID != tc.want {
				t.Fatalf("AddCatalog(%q) removal names %q, want %q", tc.id, remErr.Removed.ID, tc.want)
			}
			if !strings.Contains(err.Error(), "was removed on") || !strings.Contains(err.Error(), remErr.Removed.RemovalRef) {
				t.Fatalf("AddCatalog(%q) error = %v, want it to name the removal (date + ref)", tc.id, err)
			}
		})
	}

	// Scope exception: opencode's catalog entry is the authenticated
	// console API, not the removed keyless surface — the add goes
	// through (key test against the mock upstream).
	t.Run("opencode authenticated entry unaffected", func(t *testing.T) {
		p, err := reg.AddCatalogWithOptions(context.Background(), "opencode", "oc-key",
			CatalogAddOptions{BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("AddCatalog(opencode) = %v, want success (authenticated console entry is not the removed variant)", err)
		}
		if p.CatalogID != "opencode" {
			t.Fatalf("CatalogID = %q, want opencode", p.CatalogID)
		}
	})
}

// TestCT2AddCustomSimilarHostUnaffected — CT-2 acceptance (4): a
// non-removed provider with a similar name/host is unaffected —
// no over-blocking. A custom provider named like a removed one,
// pointed at an ordinary upstream, adds normally.
func TestCT2AddCustomSimilarHostUnaffected(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	p, err := reg.AddContext(context.Background(), "puter-like", srv.URL, "test-key")
	if err != nil {
		t.Fatalf("Add(custom, similar name) = %v, want success", err)
	}
	if p.BaseURL != srv.URL {
		t.Fatalf("BaseURL = %q, want mock URL", p.BaseURL)
	}
	if len(p.Models) != 2 {
		t.Fatalf("Models = %d, want 2 (key test + model fetch ran)", len(p.Models))
	}
}

func TestG19ApplySyncedVision(t *testing.T) {
	// Mirror of TestRegistryApplySyncedContextWindow: the sync
	// sets/clears the vision flag in memory only.
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	store := NewJSONFileStore(path)
	if err := store.Save([]ProviderConfig{{
		ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", Enabled: true,
		Models: []Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}},
	}}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	reg := NewRegistry(store)
	if err := reg.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	visionOf := func(model string) bool {
		for _, p := range reg.List() {
			for _, m := range p.Models {
				if m.ID == model {
					return m.Vision
				}
			}
		}
		return false
	}
	if err := reg.ApplySyncedVision("openai", "gpt-4o", true, true); err != nil {
		t.Fatalf("ApplySyncedVision(true): %v", err)
	}
	if !visionOf("gpt-4o") {
		t.Fatal("gpt-4o Vision still false after sync-apply true")
	}
	if visionOf("gpt-4o-mini") {
		t.Fatal("gpt-4o-mini Vision moved: other models must be untouched")
	}
	// A later feed without image input clears the flag.
	if err := reg.ApplySyncedVision("openai", "gpt-4o", false, true); err != nil {
		t.Fatalf("ApplySyncedVision(false): %v", err)
	}
	if visionOf("gpt-4o") {
		t.Fatal("gpt-4o Vision still true after sync-apply false")
	}
	if err := reg.ApplySyncedVision("openai", "no-such-model", true, true); err == nil {
		t.Fatal("ApplySyncedVision for an unknown model succeeded, want an error")
	}
	if err := reg.ApplySyncedVision("no-such-provider", "gpt-4o", true, true); err == nil {
		t.Fatal("ApplySyncedVision for an unknown provider succeeded, want an error")
	}
}
