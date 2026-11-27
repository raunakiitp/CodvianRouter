package providers

import (
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// TestAllMatchesCatalogOAuthInventory pins the registry to the
// catalog's 31 CategoryOAuth entries: every catalog OAuth provider has
// exactly one registry entry (implemented or honestly-unimplemented),
// none extra, none missing.
func TestAllMatchesCatalogOAuthInventory(t *testing.T) {
	want := map[string]bool{}
	for _, p := range catalog.All() {
		if p.Category == catalog.CategoryOAuth {
			want[p.ID] = true
		}
	}
	if len(want) != 31 {
		t.Fatalf("catalog CategoryOAuth entries = %d, want 31 (the pinned inventory)", len(want))
	}
	got := map[string]bool{}
	for _, in := range All() {
		if got[in.ID] {
			t.Errorf("registry lists %q twice", in.ID)
		}
		got[in.ID] = true
	}
	for id := range want {
		if !got[id] {
			t.Errorf("registry is missing catalog OAuth provider %q", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("registry lists %q which is NOT a catalog OAuth provider", id)
		}
	}
	if len(got) != 31 {
		t.Fatalf("registry entries = %d, want 31", len(got))
	}
}

// TestInfoHonesty pins the honesty fields: every implemented handler
// cites a doc source and carries no reason; every unimplemented entry
// explains itself; import-only providers are marked as such.
func TestInfoHonesty(t *testing.T) {
	implemented, unimpl, importOnly := 0, 0, 0
	for _, in := range All() {
		switch {
		case in.Implemented && in.ImportOnly:
			importOnly++
			if in.Reason != "" {
				t.Errorf("import-only %q should not carry a reason", in.ID)
			}
		case in.Implemented:
			implemented++
			if in.DocsURL == "" {
				t.Errorf("implemented handler %q does not cite a doc source", in.ID)
			}
			if in.Reason != "" {
				t.Errorf("implemented handler %q carries a reason: %q", in.ID, in.Reason)
			}
			if _, err := Get(in.ID); err != nil {
				t.Errorf("Get(%q): %v", in.ID, err)
			}
		default:
			unimpl++
			if in.Reason == "" {
				t.Errorf("unimplemented %q must explain why (no invented endpoints)", in.ID)
			}
			if in.DocsURL != "" {
				t.Errorf("unimplemented %q must not claim a doc source for a flow it does not implement", in.ID)
			}
			_, err := Get(in.ID)
			if !errors.Is(err, ErrNotImplemented) {
				t.Errorf("Get(%q) error = %v, want ErrNotImplemented", in.ID, err)
			}
		}
	}
	if implemented != 18 {
		t.Errorf("implemented interactive handlers = %d, want 18 (claude, github-copilot, ghe-copilot, cline, clinepass, codex, chatgpt, kimi-coding, gitlab-duo, kiro, amazon-q, cursor, xai-oauth, grok-cli, zed, zed-hosted, trae, freebuff)", implemented)
	}
	if importOnly != 2 {
		t.Errorf("import-only providers = %d, want 2 (antigravity, agy)", importOnly)
	}
	if unimpl != 11 {
		t.Errorf("unimplemented providers = %d, want 11 (the honestly-documented gaps)", unimpl)
	}
}

// TestGetRefusals checks the typed refusals: import-only providers
// point at the import path; unknown IDs are unknown.
func TestGetRefusals(t *testing.T) {
	_, err := Get("antigravity")
	if !errors.Is(err, ErrImportOnly) {
		t.Errorf("Get(antigravity) error = %v, want ErrImportOnly", err)
	}
	_, err = Get("agy")
	if !errors.Is(err, ErrImportOnly) {
		t.Errorf("Get(agy) error = %v, want ErrImportOnly", err)
	}
	_, err = Get("qoder")
	if !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Get(qoder) error = %v, want ErrNotImplemented", err)
	}
	_, err = Get("not-a-provider")
	if err == nil || errors.Is(err, ErrNotImplemented) || errors.Is(err, ErrImportOnly) {
		t.Errorf("Get(not-a-provider) error = %v, want an unknown-provider error", err)
	}
}

// TestImplementedFlowTypes pins the flow classification.
func TestImplementedFlowTypes(t *testing.T) {
	want := map[string]Flow{
		"claude":         FlowPKCE,
		"codex":          FlowPKCE,
		"chatgpt":        FlowPKCE,
		"github-copilot": FlowDeviceCode,
		"ghe-copilot":    FlowDeviceCode,
		"cline":          FlowDeviceCode,
		"clinepass":      FlowDeviceCode,
		"kimi-coding":    FlowDeviceCode,
		"gitlab-duo":     FlowBrowserSignIn,
		"kiro":           FlowDeviceCode,
		"amazon-q":       FlowDeviceCode,
		"cursor":         FlowBrowserSignIn,
		"xai-oauth":      FlowDeviceCode,
		"grok-cli":       FlowDeviceCode,
		"zed":            FlowBrowserSignIn,
		"zed-hosted":     FlowBrowserSignIn,
		"trae":           FlowBrowserSignIn,
		"freebuff":       FlowDeviceCode,
	}
	for _, in := range ImplementedInfos() {
		if in.ImportOnly {
			continue
		}
		if want[in.ID] != in.Flow {
			t.Errorf("handler %q flow = %q, want %q", in.ID, in.Flow, want[in.ID])
		}
		delete(want, in.ID)
	}
	for id := range want {
		t.Errorf("no implemented handler for %q", id)
	}
}
