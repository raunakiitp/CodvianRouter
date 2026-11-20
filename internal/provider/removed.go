// Removed-provider enforcement (G19 Area 1, gap-track CT-2).
//
// The removal register itself lives in internal/catalog
// (removed.go): providers removed at their operator's request
// must never come back. That file's CheckRemovedBlocklist is the
// SEED-data guard — it scans the static catalog and fails when a
// removed provider re-enters the seed. It cannot judge a
// registry provider or a custom base URL, so the runtime
// enforcement lives here, on the registry's own types:
//
//   - IsRemovedVariant: the per-provider check — is THIS
//     registry entry the removed variant? It mirrors
//     catalog.RemovedProvider.Matches, including the keyless-only
//     nuance (an authenticated opencode entry is a different,
//     legitimate provider; only the keyless "OpenCode Free"
//     surface was removed). Pool construction (chainFor, the auto
//     ranker, the auto catalog, the candidate-pool endpoint)
//     skips a removed variant; the add path rejects it.
//   - IsRemovedBaseURL: the custom-provider check — does this
//     base URL belong to a removed provider (host, or host+path
//     for path-scoped removals like opencode.ai/zen/v1)? The add
//     path rejects it, so a removed provider cannot be re-added
//     as a custom provider with an explicit base URL.
package provider

import (
	"net/url"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

// IsRemovedVariant reports whether this registry provider IS the
// removed variant a removal-register entry records, with the
// entry. The match is on CatalogID (falling back to the
// provider's own ID) against the removed ID/alias. A whole-
// integration removal matches any entry carrying the ID; a
// keyless-only removal matches only an entry with no credential
// surface at all (no stored keys, no legacy API key, no auth
// endpoint) — an authenticated entry under the same ID is a
// different provider and does not match.
func (p *Provider) IsRemovedVariant() (catalog.RemovedProvider, bool) {
	if p == nil {
		return catalog.RemovedProvider{}, false
	}
	for _, r := range catalog.RemovedProviders {
		if p.CatalogID != r.ID && p.CatalogID != r.Alias && p.ID != r.ID && p.ID != r.Alias {
			continue
		}
		if !r.KeylessOnly {
			return r, true
		}
		if len(p.Keys) == 0 && p.APIKeyEncrypted == "" && p.AuthEndpoint == "" {
			return r, true
		}
	}
	return catalog.RemovedProvider{}, false
}

// IsRemovedBaseURL reports whether rawURL's host (or host+path
// prefix, for path-scoped removals) belongs to a removed
// provider, with the entry. An unparsable URL or one with no
// host is not a removed URL — it fails validation elsewhere,
// never here. The match mirrors catalog.IsRemovedDomain
// (exact host, sub-domain suffix, or host+path prefix).
func IsRemovedBaseURL(rawURL string) (catalog.RemovedProvider, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return catalog.RemovedProvider{}, false
	}
	h := strings.ToLower(u.Host)
	hp := strings.ToLower(u.Host + u.Path)
	for _, r := range catalog.RemovedProviders {
		for _, d := range r.Domains {
			dl := strings.ToLower(d)
			// Host match (exact or sub-domain) OR host+path
			// prefix (path-scoped removals like
			// opencode.ai/zen/v1). The host check runs on
			// the host alone: a path suffix would defeat
			// the sub-domain match ("api.puter.com/v1"
			// does not end in ".puter.com").
			if h == dl || strings.HasSuffix(h, "."+dl) || hp == dl || strings.HasPrefix(hp, dl+"/") {
				return r, true
			}
		}
	}
	return catalog.RemovedProvider{}, false
}
