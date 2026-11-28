// Removed providers (parity tracker Area 3, "Deprecated / retired handling").
//
// the sector baseline keeps a durable removal register (REMOVED_PROVIDERS.md):
// providers removed at their operator's request must never come back
// by accident — a contributor finding an old fork, a cached tarball
// or a "restore provider X" request needs one place that says do not
// reintroduce. This file is our copy of that register, with the same
// rule: an entry here must never be re-added to the catalog in the
// removed form, and CheckRemovedBlocklist fails the build if one
// shows up again.
//
// Source: reference removal register (REMOVED_PROVIDERS.md), version
// 3.8.51, last updated 2026-10-10 (read 2026-11-19). Only the
// identifiers, domains, dates and removal references are copied —
// facts about a removal, not reference code.
//
// Scope matters (read the Notes before enforcing): the `opencode`
// removal covers ONLY the keyless "OpenCode Free" provider. The
// authenticated OpenCode console API (our `opencode` entry,
// verified-from-docs, API key, opencode.ai/inference/...) is a
// different surface and is NOT removed — Matches() encodes that
// distinction, so the blocklist never nukes a legitimate entry.
package catalog

import "strings"

// RemovedProvider is one provider the catalog must never carry in
// the removed form.
type RemovedProvider struct {
	// ID is the removed provider ID.
	ID string
	// Alias is the removed provider's alias, if any.
	Alias string
	// Domains are the removed provider's domains.
	Domains []string
	// RemovedOn is the removal date (YYYY-MM-DD).
	RemovedOn string
	// RequestedBy names who asked for the removal.
	RequestedBy string
	// RemovalRef is the removal PR (or issue) reference.
	RemovalRef string
	// Source is where the removal is recorded.
	Source string
	// Scope says exactly what was removed, so enforcement does not
	// overreach (see the opencode note in the file header).
	Scope string
	// KeylessOnly is true when only the keyless / no-auth variant
	// was removed and an authenticated entry under the same ID is
	// a different, legitimate provider.
	KeylessOnly bool
}

// RemovedProviders is the removal register. Keep it in removal
// order; add a row the same day a takedown is honoured, never later.
var RemovedProviders = []RemovedProvider{
	{
		ID:          "puter",
		Alias:       "pu",
		Domains:     []string{"puter.com"},
		RemovedOn:   "2026-08-12",
		RequestedBy: "Puter's owner (Nariman Jelveh)",
		RemovalRef:  "PR #10210",
		Source:      "reference removal register (REMOVED_PROVIDERS.md)",
		Scope:       "API-key provider, whole integration removed.",
	},
	{
		ID:          "theoldllm",
		Alias:       "tllm",
		Domains:     []string{"theoldllm.com", "theoldllm.vercel.app"},
		RemovedOn:   "2026-09-02",
		RequestedBy: "The service operator (support email)",
		RemovalRef:  "PR #12440",
		Source:      "reference removal register (REMOVED_PROVIDERS.md)",
		Scope:       "Keyless provider, whole integration removed.",
	},
	{
		ID:          "gemini-business",
		Alias:       "gembiz",
		Domains:     []string{"business.gemini.google"},
		RemovedOn:   "2026-09-21",
		RequestedBy: "Project owner decision (the sector baseline issue #14217)",
		RemovalRef:  "PR #14467",
		Source:      "reference removal register (REMOVED_PROVIDERS.md)",
		Scope:       "Cookie-based web provider; Google retired the endpoint the integration targeted.",
	},
	{
		ID:          "suno",
		Alias:       "suno",
		Domains:     []string{"studio-api.suno.ai", "studio-api-prod.suno.com"},
		RemovedOn:   "2026-09-21",
		RequestedBy: "Project owner decision (the sector baseline issue #14224)",
		RemovalRef:  "PR #14468",
		Source:      "reference removal register (REMOVED_PROVIDERS.md)",
		Scope:       "Cookie-auth provider; kie.ai-hosted suno models (kie/suno-v4.0, kie/suno-v3.5) are unaffected and remain supported.",
	},
	{
		ID:          "opencode",
		Alias:       "oc",
		Domains:     []string{"opencode.ai/zen/v1"},
		RemovedOn:   "2026-10-10",
		RequestedBy: "Preventive removal (project owner decision), honoring the OpenCode operator's public removal request to the upstream 9router project (2026-09-19)",
		RemovalRef:  "PR #16310",
		Source:      "reference removal register (REMOVED_PROVIDERS.md)",
		Scope:       "ONLY the keyless \"OpenCode Free\" provider was removed. Authenticated opencode-zen and opencode-go, all Zen catalog models and the OpenCode client integrations remain — so an authenticated opencode entry is NOT a reintroduction.",
		KeylessOnly: true,
	},
}

// IsRemoved reports whether id or alias is a removed provider ID,
// with the register entry. It answers "is this identifier on the
// removal register", not "is this catalog entry the removed
// variant" — for that, use RemovedProvider.Matches.
func IsRemoved(id string) (RemovedProvider, bool) {
	for _, r := range RemovedProviders {
		if r.ID == id || (r.Alias != "" && r.Alias == id) {
			return r, true
		}
	}
	return RemovedProvider{}, false
}

// Matches reports whether catalog entry p IS the removed variant
// this row records. A keyless-only removal matches only a keyless
// entry (no-auth category, or no credential surface at all); an
// authenticated entry under the same ID is a different provider
// and does not match. A whole-integration removal matches any
// entry carrying the ID.
func (r RemovedProvider) Matches(p Provider) bool {
	if p.ID != r.ID && p.Alias != r.Alias {
		return false
	}
	if !r.KeylessOnly {
		return true
	}
	// Keyless-only removal: only a keyless entry is the removed
	// variant. An API-key entry (or any entry with a base URL and
	// a credential surface) is the authenticated provider.
	return p.Category == CategoryNoAuth || (len(p.BaseURLs) == 0 && p.Category != CategoryAPIKey)
}

// IsRemovedDomain reports whether the host (or host+path prefix)
// belongs to a removed provider.
func IsRemovedDomain(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	for _, r := range RemovedProviders {
		for _, d := range r.Domains {
			dl := strings.ToLower(d)
			if h == dl || strings.HasSuffix(h, "."+dl) || strings.HasPrefix(h, dl+"/") {
				return true
			}
		}
	}
	return false
}

// CheckRemovedBlocklist scans the full catalog and fails when a
// removed provider shows up again in the removed form. It is the
// regression guard: call it from tests only. It scans the static
// seed catalog and cannot judge an incoming provider — the
// per-input checks (IsRemovedDomain / Matches on the new entry)
// are the real enforcement on the add-provider path, and calling
// this there was deliberately rejected.
func CheckRemovedBlocklist() error {
	for _, p := range All() {
		for _, r := range RemovedProviders {
			if r.Matches(p) {
				return &RemovedProviderError{Entry: p, Removed: r}
			}
		}
	}
	return nil
}

// RemovedProviderError is returned when a removed provider
// reappears in the catalog.
type RemovedProviderError struct {
	Entry   Provider
	Removed RemovedProvider
}

func (e *RemovedProviderError) Error() string {
	return "catalog: provider " + e.Entry.ID + " was removed on " + e.Removed.RemovedOn +
		" (" + e.Removed.RemovalRef + ") and must not be reintroduced; see " + e.Removed.Source
}
