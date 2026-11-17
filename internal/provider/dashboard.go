package provider

import "github.com/raunakiitp/codvian-router/internal/catalog"

// DashboardProvider is the safe-to-display summary of one configured
// provider, for the dashboard (GET /dashboard/providers). It carries
// only operator-visible facts — verification status, wire format and
// auth kind — and never a secret: no API key, no base URL, no
// credential material of any kind.
type DashboardProvider struct {
	// ID is the registry ID; Name is the display name.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Enabled is whether the provider is currently routed to.
	Enabled bool `json:"enabled"`
	// WireFormat is the wire the provider speaks ("openai",
	// "anthropic", "gemini", "cohere", "bedrock-converse", ...).
	WireFormat string `json:"wire_format"`
	// Verification is the catalog entry's verification status
	// ("verified-from-docs", "from-reference-metadata-only",
	// "unverified"). Custom providers are "unverified" — the router
	// has not checked their facts against any documentation.
	Verification string `json:"verification"`
	// AuthKind is how the credential reaches the upstream
	// ("bearer", "x-api-key", "x-goog-api-key", "api-key",
	// "exchange", "none", ...). For a catalog provider it is the
	// auth kind of the surface the stored credentials are for; for
	// a hand-added provider it is "bearer" (the legacy default).
	// Empty only when a catalog entry cannot be resolved at all.
	AuthKind string `json:"auth_kind"`
	// AuthEndpoint / AuthVariant name the catalog surface the stored
	// credentials are for ("" = the entry's default surface).
	AuthEndpoint string `json:"auth_endpoint,omitempty"`
	AuthVariant  string `json:"auth_variant,omitempty"`
	// CatalogID links the provider to its catalog entry; empty for
	// fully-custom providers.
	CatalogID string `json:"catalog_id,omitempty"`
	// Models is how many models the provider currently offers.
	Models int `json:"models"`
}

// DashboardProviders returns live summaries of every configured
// provider, in registry (insertion) order. An empty registry returns
// an empty (non-nil) slice, never nil.
func (r *Registry) DashboardProviders() []DashboardProvider {
	list := r.List()
	out := make([]DashboardProvider, 0, len(list))
	for _, p := range list {
		if p == nil {
			continue
		}
		// Empty verification means the facts were never checked
		// (providers stored before verification was recorded):
		// report that as unverified rather than a blank.
		verification := p.Verification
		if verification == "" {
			verification = string(catalog.VerificationUnverified)
		}
		out = append(out, DashboardProvider{
			ID:           p.ID,
			Name:         p.Name,
			Enabled:      p.Enabled,
			WireFormat:   p.WireFormat,
			Verification: verification,
			AuthKind:     dashboardAuthKind(p),
			AuthEndpoint: p.AuthEndpoint,
			AuthVariant:  p.AuthVariant,
			CatalogID:    p.CatalogID,
			Models:       len(p.Models),
		})
	}
	return out
}

// dashboardAuthKind resolves the auth kind for one stored provider:
// the catalog entry's endpoint auth when the provider came from the
// catalog, the legacy Bearer default for hand-added providers.
func dashboardAuthKind(p *Provider) string {
	if p.CatalogID == "" {
		return string(catalog.AuthBearer)
	}
	entry, err := lookupCatalog(p.CatalogID)
	if err != nil {
		return ""
	}
	if ep, ok := entry.Endpoint(p.AuthEndpoint, p.AuthVariant); ok {
		return string(ep.AuthKind)
	}
	return string(entry.DefaultEndpointAuth().AuthKind)
}
