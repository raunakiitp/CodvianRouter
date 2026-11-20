package provider

import (
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
)

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
	// KeyCreationURL is where a user creates an API key for this
	// provider (the provider's key-management / signup page, from
	// the catalog entry's KeyCreationURL — see
	// internal/catalog/keycreation.go). "" when the catalog entry
	// records none, or for fully-custom providers. It is a public
	// page URL, never a credential and never the provider's API
	// base URL, so it is safe to display and to link from the
	// dashboard ("Get API key →").
	KeyCreationURL string `json:"key_creation_url,omitempty"`
	// Models is how many models the provider currently offers.
	Models int `json:"models"`
	// Keys is the provider's API-key pool (CHECKLIST 175): one
	// entry per key, in pool order — ID, role and fingerprint
	// only, never ciphertext or plaintext. A provider stored
	// before pools existed reports its single synthesized legacy
	// key here, so the dashboard never shows a provider with no
	// keys while it routes.
	Keys []DashboardProviderKey `json:"keys,omitempty"`
}

// DashboardProviderKey is one key of a provider's pool, safe to
// display (CHECKLIST 175): ID, role and fingerprint only — never
// ciphertext, never plaintext. Fingerprint is the first 12 hex of
// SHA-256(plaintext), computed at add time; it is "" for a legacy
// key synthesized from a stored ciphertext (the fingerprint cannot
// be computed without the plaintext), where the dashboard shows the
// key ID instead.
type DashboardProviderKey struct {
	ID          string    `json:"id"`
	Role        string    `json:"role"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// DashboardProviders returns live summaries of every configured
// provider, in registry (insertion) order. An empty registry returns
// an empty (non-nil) slice, never nil.
func (r *Registry) DashboardProviders() []DashboardProvider {
	list := r.List()
	out := make([]DashboardProvider, 0, len(list))
	// Key-creation pages come from the catalog entry the provider
	// was added from (catalog.All applies the keycreation.go seed
	// fill). One lookup map per call, not one All() per provider.
	keyPages := map[string]string{}
	for _, e := range catalog.All() {
		if e.KeyCreationURL != "" {
			keyPages[e.ID] = e.KeyCreationURL
		}
	}
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
			ID:             p.ID,
			Name:           p.Name,
			Enabled:        p.Enabled,
			WireFormat:     p.WireFormat,
			Verification:   verification,
			AuthKind:       dashboardAuthKind(p),
			AuthEndpoint:   p.AuthEndpoint,
			AuthVariant:    p.AuthVariant,
			CatalogID:      p.CatalogID,
			KeyCreationURL: keyPages[p.CatalogID],
			Models:         len(p.Models),
			Keys:           dashboardKeys(p),
		})
	}
	return out
}

// dashboardKeys converts a provider's effective key pool into its
// display-safe form: ID, role, fingerprint, creation time — never
// ciphertext. A legacy single-key provider reports its one
// synthesized key (fingerprint "" — see DashboardProviderKey).
func dashboardKeys(p *Provider) []DashboardProviderKey {
	keys := p.EffectiveKeys()
	out := make([]DashboardProviderKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, DashboardProviderKey{
			ID:          k.ID,
			Role:        k.EffectiveRole(),
			Fingerprint: k.Fingerprint,
			CreatedAt:   k.CreatedAt,
		})
	}
	return out
}

// DashboardModel is one model in the dashboard catalog feed
// (GET /dashboard/models): identity and best-effort metadata from
// the registry — never a secret, never invented. ContextWindow and
// prices are 0 when unknown (most upstreams do not report pricing via
// /v1/models): the dashboard shows "—" for those, never a guessed
// number. Tools is the tri-state tool capability from the model
// entry (nil = unknown, shown as "—"; never guessed from the id).
type DashboardModel struct {
	ID             string  `json:"id"`
	ProviderID     string  `json:"provider_id"`
	ProviderName   string  `json:"provider_name"`
	WireFormat     string  `json:"wire_format"`
	Verification   string  `json:"verification"`
	Enabled        bool    `json:"enabled"`
	ContextWindow  int     `json:"context_window"`
	InputPriceUSD  float64 `json:"input_price_usd"`
	OutputPriceUSD float64 `json:"output_price_usd"`
	// Tools is the model's tool/function-call capability
	// (CT-1): nil means unknown, true confirmed capable,
	// false confirmed incapable — recorded from the live
	// probe / models.dev where available, or operator-set.
	Tools *bool `json:"tools,omitempty"`
	// Category is how the user connects to this model's provider
	// (the catalog category: api-key, free, local, no-auth,
	// oauth, ...; "custom" for a hand-added provider with no
	// catalog entry). It is the `category` filter of
	// GET /v1/models and GET /dashboard/models.
	Category string `json:"category"`
	// Shortlisted is true when the operator pinned this model in
	// the config `shortlist` (CHECKLIST 199-200). The registry
	// does not know the shortlist — the server sets this field
	// on the feed rows after fetching them.
	Shortlisted bool `json:"shortlisted,omitempty"`
	// Deprecated is true when the operator flagged the model retired
	// OR the catalog's retired-model table lists the ID. The
	// dashboard shows it as deprecated (hidden by default); the
	// request path never blocks on it.
	Deprecated bool `json:"deprecated"`
}

// DashboardModels returns every model of every configured provider,
// in registry (insertion) order, provider by provider. An empty
// registry returns an empty (non-nil) slice, never nil.
func (r *Registry) DashboardModels() []DashboardModel {
	list := r.List()
	out := make([]DashboardModel, 0)
	for _, p := range list {
		if p == nil {
			continue
		}
		verification := p.Verification
		if verification == "" {
			verification = string(catalog.VerificationUnverified)
		}
		for _, m := range p.Models {
			if m.ID == "" {
				continue
			}
			out = append(out, DashboardModel{
				ID: m.ID, ProviderID: p.ID, ProviderName: p.Name,
				WireFormat: p.WireFormat, Verification: verification,
				Enabled: p.Enabled, ContextWindow: m.ContextWindow,
				InputPriceUSD: m.InputPriceUSD, OutputPriceUSD: m.OutputPriceUSD,
				Tools:      m.Tools,
				Category:   CategoryForProvider(p),
				Deprecated: m.Deprecated || isCatalogDeprecated(m.ID),
			})
		}
	}
	return out
}

// CategoryForProvider resolves the catalog category for one stored
// provider ("api-key", "free", "local", "no-auth", "oauth", ... —
// see catalog.Category). A hand-added provider with no catalog entry,
// or an entry that cannot be resolved, is "custom": the filter value
// for providers the catalog does not know.
func CategoryForProvider(p *Provider) string {
	if p.CatalogID != "" {
		if entry, err := lookupCatalog(p.CatalogID); err == nil && entry.Category != "" {
			return string(entry.Category)
		}
	}
	return string(catalog.CategoryCustom)
}

// isCatalogDeprecated reports whether the catalog's retired-model
// table lists id. The dashboard and the model list consult it
// alongside the operator-set Model.Deprecated flag, so a retired
// model is marked deprecated even when the stored provider was
// fetched before the retirement was known.
func isCatalogDeprecated(id string) bool {
	_, ok := catalog.IsDeprecated(id)
	return ok
}

// AuthKindFor returns the auth-kind string for one stored provider
// (see dashboardAuthKind): the catalog entry's endpoint auth when
// the provider came from the catalog, else the legacy Bearer kind.
// It is the main package's input to the router's breaker-profile
// mapping (M4.9): an empty result means "unknown", and the router
// maps it to the API-key profile.
func AuthKindFor(p *Provider) string {
	return dashboardAuthKind(p)
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
