package catalog

import "testing"

// TestRemovedAddRefusalSemantics pins the predicate the registry's
// add/load enforcement (CT-2) relies on: a whole-integration removal
// is refusable by ID alone, before any catalog lookup — the removed
// providers are not in the catalog, so the lookup cannot judge
// them. The one keyless-only removal (opencode) is NOT refusable by
// ID alone: the ID also names the legitimate authenticated console
// entry, which only RemovedProvider.Matches can tell apart.
func TestRemovedAddRefusalSemantics(t *testing.T) {
	for _, r := range RemovedProviders {
		_, ok := IsRemoved(r.ID)
		if !ok {
			t.Fatalf("IsRemoved(%q) = false, want the register entry", r.ID)
		}
	}
	for _, id := range []string{"puter", "theoldllm", "gemini-business", "suno"} {
		r, ok := IsRemoved(id)
		if !ok {
			t.Fatalf("IsRemoved(%q) = false", id)
		}
		if r.KeylessOnly {
			t.Fatalf("IsRemoved(%q).KeylessOnly = true, want false (whole-integration removal is refusable by ID)", id)
		}
	}
	r, ok := IsRemoved("opencode")
	if !ok {
		t.Fatal("IsRemoved(opencode) = false")
	}
	if !r.KeylessOnly {
		t.Fatal("IsRemoved(opencode).KeylessOnly = false, want true (only the keyless variant was removed)")
	}
}

// TestRemovedProvidersBlocklist — internal parity tracker:588, PARTIAL row: no
// provider-removal blocklist / takedown process existed. The
// register (removed.go) mirrors the reference router's REMOVED_PROVIDERS.md
// (5 removals) with a regression guard.
func TestRemovedProvidersBlocklist(t *testing.T) {
	if len(RemovedProviders) != 5 {
		t.Fatalf("removed providers = %d, want 5 (the reference router REMOVED_PROVIDERS.md v3.8.51)", len(RemovedProviders))
	}
	for id, alias := range map[string]string{
		"puter": "pu", "theoldllm": "tllm", "gemini-business": "gembiz",
		"suno": "suno", "opencode": "oc",
	} {
		r, ok := IsRemoved(id)
		if !ok || r.ID != id || r.Alias != alias {
			t.Fatalf("IsRemoved(%q) = %+v, %v; want the register entry", id, r, ok)
		}
		if _, ok := IsRemoved(alias); !ok {
			t.Fatalf("IsRemoved(%q) by alias failed", alias)
		}
		if r.RemovedOn == "" || r.Source == "" || r.RemovalRef == "" {
			t.Fatalf("register entry %q is missing removal metadata", id)
		}
	}
	if _, ok := IsRemoved("openai"); ok {
		t.Fatal("openai reported as removed")
	}
	for _, p := range All() {
		for _, r := range RemovedProviders {
			if r.Matches(p) {
				t.Fatalf("catalog entry %q matches removed provider %q (%s)", p.ID, r.ID, r.Scope)
			}
		}
	}
	if err := CheckRemovedBlocklist(); err != nil {
		t.Fatalf("CheckRemovedBlocklist: %v", err)
	}
	// The opencode scope exception: the keyless OpenCode Free
	// provider was removed, but the authenticated console API is
	// a different surface and must NOT match.
	real, ok := ByID("opencode")
	if !ok {
		t.Fatal("opencode (authenticated console entry) missing from catalog")
	}
	r, _ := IsRemoved("opencode")
	if r.Matches(real) {
		t.Fatal("blocklist matched the authenticated opencode entry: the removal covers only the keyless free variant")
	}
	keyless := Provider{ID: "opencode", Category: CategoryNoAuth}
	if !r.Matches(keyless) {
		t.Fatal("blocklist did not match a keyless opencode entry: the removed variant must match")
	}
	// Domain guard.
	for host, want := range map[string]bool{
		"puter.com": true, "www.puter.com": true,
		"business.gemini.google": true, "studio-api.suno.ai": true,
		"openrouter.ai": false, "notputer.com": false,
	} {
		if got := IsRemovedDomain(host); got != want {
			t.Fatalf("IsRemovedDomain(%q) = %v, want %v", host, got, want)
		}
	}
}
