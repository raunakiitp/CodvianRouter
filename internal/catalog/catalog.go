// Package catalog is the Codvian Router provider catalog: the list of
// LLM providers the router knows about, how to authenticate to each one,
// and which wire format each one speaks.
//
// The catalog is deliberately bigger than any single competitor's list:
// it starts from the providers the reference router documents (394 entries, taken
// from its reference metadata — verification status
// "from-reference-metadata-only"), adds providers verified against their
// own official documentation ("verified-from-docs"), and — the real
// lead — lets the user add ANY provider as a custom provider (any base
// URL + key), so coverage is never capped by this list.
//
// HONESTY RULES (2026-11-17, non-negotiable):
//   - Never invent a base URL, key prefix, price, or free-tier claim.
//     Unknown stays empty ("") or WireUnknown / VerificationUnverified.
//   - Every entry carries a VerificationStatus saying how its facts were
//     established. A base URL taken from reference metadata is NOT
//     presented as verified.
//   - Free-tier notes exist only when verified from the provider's own
//     docs. There is no "~1.62B free tokens/month" style headline here:
//     the catalog reports per-provider facts, never an aggregate
//     invented from third-party free tiers.
//   - Web-cookie / subscription-OAuth providers are catalogued with
//     ToSGrey=true and are never recommended: they rely on session
//     cookies or someone else's subscription login, which is fragile
//     and against most providers' terms.
//
// Auth is resolved per (provider, endpoint), never per provider alone:
// the same key can need Authorization: Bearer on one surface and
// x-api-key on another (DeepSeek, Fireworks, OpenRouter), and some
// credentials are not static headers at all but exchanges
// (Vertex service-account JSON, Bedrock SigV4, OpenRouter JWT). The
// resolver in auth.go never fakes an exchange as a static header.
//
// This package is stdlib-only and has no dependency on internal/provider,
// internal/config, or internal/server, so it compiles standalone. It is
// data + auth resolution only: wiring the catalog into the registry and
// the request path is a later step (see CHECKLIST.md).
package catalog

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// VerificationStatus says how an entry's facts were established. It is
// the honesty label for the whole catalog: consumers (dashboard, docs,
// routing decisions) must show it, never a bare provider count.
type VerificationStatus string

const (
	// VerificationDocs means the entry's base URL, auth scheme, and any
	// free-tier note were checked against the provider's own official
	// documentation (research pass 2026-11-17).
	VerificationDocs VerificationStatus = "verified-from-docs"

	// VerificationReference means the entry was taken from the reference router
	// reference metadata (names, website, category, hint text). Its base
	// URL and auth were NOT independently verified.
	VerificationReference VerificationStatus = "from-reference-metadata-only"

	// VerificationUnverified means nothing about the entry was verified
	// — name only. Such entries must never be routed to.
	VerificationUnverified VerificationStatus = "unverified"
)

// AuthKind is one way a credential reaches an upstream.
type AuthKind string

const (
	// AuthBearer is `Authorization: Bearer <key>` — the default for
	// ~80% of providers (OpenAI-compatible surface).
	AuthBearer AuthKind = "bearer"

	// AuthXAPIKey is `x-api-key: <key>` (Anthropic legacy surface,
	// DeepSeek's Anthropic surface).
	AuthXAPIKey AuthKind = "x-api-key"

	// AuthAPIKey is `api-key: <key>` (Azure OpenAI legacy REST).
	AuthAPIKey AuthKind = "api-key"

	// AuthXGoogAPIKey is `x-goog-api-key: <key>` (Google AI Studio).
	AuthXGoogAPIKey AuthKind = "x-goog-api-key"

	// AuthXFireworksAPIKey is `X-Fireworks-Api-Key: <key>` (Fireworks
	// alternate header; Bearer also works).
	AuthXFireworksAPIKey AuthKind = "x-fireworks-api-key"

	// AuthNone means the endpoint takes no credential (local runtimes,
	// public no-auth endpoints). SDKs may still need a placeholder key;
	// that is a client concern, not auth.
	AuthNone AuthKind = "none"

	// AuthOAuth is an OAuth / subscription flow (device code, PKCE):
	// the credential is a short-lived Bearer token minted by a login
	// flow, not a stored API key. See Exchange for how it is obtained.
	AuthOAuth AuthKind = "oauth"

	// AuthCookie is web-session cookie auth (session cookies + browser
	// fingerprinting). ToS-grey: catalogued for completeness, never
	// recommended, never a routing default.
	AuthCookie AuthKind = "cookie"

	// AuthExchange means the stored credential is NOT a bearer token:
	// it must be exchanged first (service-account JSON -> OAuth token,
	// AWS creds -> SigV4 signature, OpenRouter -> 15-minute JWT).
	// The resolver never fabricates a static header for these.
	AuthExchange AuthKind = "exchange"

	// AuthCustom is a user-defined header name (custom providers).
	AuthCustom AuthKind = "custom"
)

// WireFormat is the request shape a provider natively speaks.
type WireFormat string

const (
	// WireOpenAI is OpenAI Chat Completions (/v1/chat/completions).
	WireOpenAI WireFormat = "openai"
	// WireAnthropic is Anthropic Messages (/v1/messages).
	WireAnthropic WireFormat = "anthropic"
	// WireGemini is Gemini generateContent.
	WireGemini WireFormat = "gemini"
	// WireCohere is Cohere v2 native chat (/v2/chat).
	WireCohere WireFormat = "cohere"
	// WireBedrock is Bedrock Converse (native, SigV4-signed).
	WireBedrock WireFormat = "bedrock-converse"
	// WireOllama is Ollama native (/api/chat).
	WireOllama WireFormat = "ollama"
	// WireUnknown means the wire format is not known — never guessed.
	WireUnknown WireFormat = "unknown"
)

// ToolCalling says how a provider performs tool/function calling.
// It mirrors the `toolCalling` marker in the the reference router reference
// metadata ("supported" / "emulated" / "native" there; "none" is
// recorded as unknown here with the reason in Notes).
type ToolCalling string

const (
	// ToolCallingUnknown means the reference states nothing about
	// tool calling for this provider. Unknown never counts as tool
	// support — see SupportsTools. "" (unset) means the same.
	ToolCallingUnknown ToolCalling = "unknown"

	// ToolCallingSupported means the provider natively supports
	// tool/function calling (reference: "native").
	ToolCallingSupported ToolCalling = "supported"

	// ToolCallingEmulated means tool calls are prompt-emulated:
	// the model is asked to emit tool calls as text and they are
	// parsed back out (the reference router webTools.ts), rather than the
	// provider executing native function calling. Emulated is NOT
	// tool support and must never satisfy a tool-capable filter or
	// an auto-pool capability check (CT-4 / GAP-TRACK entry 403).
	ToolCallingEmulated ToolCalling = "emulated"
)

// Category groups providers by how the user connects to them. It mirrors
// the connection-type breakdown other routers publish (API key, OAuth,
// local, no-auth, web-cookie, ...) so counts stay comparable.
type Category string

const (
	CategoryAPIKey        Category = "api-key"
	CategoryFree          Category = "free"
	CategoryOAuth         Category = "oauth"
	CategoryLocal         Category = "local"
	CategoryNoAuth        Category = "no-auth"
	CategoryWebCookie     Category = "web-cookie"
	CategorySearch        Category = "search"
	CategoryAudio         Category = "audio"
	CategoryUpstreamProxy Category = "upstream-proxy"
	CategoryCloudAgent    Category = "cloud-agent"
	CategorySystem        Category = "system"
	CategoryCustom        Category = "custom"
)

// ExchangeAuth describes how a non-static credential is obtained. It is
// documentation of a real exchange, never a fake header: the resolver
// returns this to the caller instead of inventing an Authorization value.
type ExchangeAuth struct {
	// Kind names the exchange, e.g. "service-account-json",
	// "sigv4", "jwt", "oauth-device-code", "oauth-pkce", "entra-id".
	Kind string
	// Description says how the credential is obtained, in one or two
	// sentences, with the properties that matter (what is stored, what
	// is minted, where the minting happens).
	Description string
	// TokenTTL is the minted token's lifetime ("1h", "15m") or ""
	// when the exchange does not mint a bearer token (SigV4 signs
	// each request instead).
	TokenTTL string
	// DocURL is the official doc describing the exchange.
	DocURL string
}

// Endpoint names. Endpoints are the unit auth is resolved per: the same
// provider can demand a different header, base URL, or exchange on each
// surface.
const (
	// EndpointChat is the provider's chat-completions surface
	// (OpenAI-compatible /v1/chat/completions, or the provider's
	// native chat endpoint).
	EndpointChat = "chat"
	// EndpointAnthropic is an Anthropic-Messages surface
	// (/v1/messages or the provider's /anthropic base).
	EndpointAnthropic = "anthropic"
	// EndpointMessages is the Anthropic /v1/messages path name, kept
	// as an alias for entries that name the surface "messages".
	EndpointMessages = "messages"
	// EndpointResponses is the OpenAI Responses surface (/v1/responses).
	EndpointResponses = "responses"
	// EndpointModels is the model-list surface (/v1/models).
	EndpointModels = "models"
	// EndpointGenerate is a Gemini generateContent surface.
	EndpointGenerate = "generate"
	// EndpointConverse is the Bedrock Converse surface.
	EndpointConverse = "converse"
	// EndpointImages is an image-generation surface.
	EndpointImages = "images"
	// EndpointEmbeddings is an embeddings surface.
	EndpointEmbeddings = "embeddings"
	// EndpointRerank is a rerank surface.
	EndpointRerank = "rerank"
	// EndpointNative is a provider's native (non-OpenAI) chat surface
	// when it has no better-known name (Cohere /v2/chat, Ollama /api).
	EndpointNative = "native"
)

// EndpointAuth describes how to authenticate ONE endpoint surface of a
// provider: which base URL, which header, which extra headers, and —
// for exchange kinds — how the credential is obtained.
type EndpointAuth struct {
	// Endpoint is the surface name (see Endpoint* constants).
	Endpoint string
	// Variant distinguishes two auth options for the same surface,
	// e.g. "default" vs "alt-header" (Fireworks Bearer vs
	// X-Fireworks-Api-Key). "" means "default".
	Variant string
	// BaseURL is this surface's base URL. "" means "use the
	// provider's first BaseURLs entry". Templates are allowed and
	// are called out in Notes (e.g. Azure {resource}, Vertex
	// {LOCATION}, Cloudflare {account_id}).
	BaseURL string
	// AuthKind is how the credential is sent on this surface.
	AuthKind AuthKind
	// HeaderName is the header carrying the credential ("Authorization"
	// for Bearer, "x-api-key", "api-key", ...). "" for AuthNone and
	// AuthExchange (no static header exists).
	HeaderName string
	// ExtraHeaders are required static headers for this surface,
	// e.g. {"anthropic-version": "2026-10-21"}.
	ExtraHeaders map[string]string
	// Exchange is non-nil when AuthKind is AuthExchange or AuthOAuth:
	// how the credential is obtained.
	Exchange *ExchangeAuth
}

// Provider is one catalog entry: a known upstream LLM backend.
type Provider struct {
	// ID is the stable lowercase kebab-case identifier
	// ("openai", "anthropic", "deepseek").
	ID string
	// Name is the display name.
	Name string
	// Alias is a short alternate ID users may type ("gpt", "cc").
	Alias string
	// Website is the provider's homepage.
	Website string
	// Category is how the user connects (see Category* constants).
	Category Category
	// BaseURLs are the provider's API bases. Empty means unknown —
	// never invented. Entries may hold templates ({resource},
	// {region}, {account_id}, {LOCATION}) when the provider's own
	// docs define the URL that way; Notes says so.
	BaseURLs []string
	// WireFormats lists the request shapes the provider natively
	// speaks, in preference order.
	WireFormats []WireFormat
	// Endpoints is the per-(provider, endpoint) auth table. Empty
	// means "default": Bearer on BaseURLs[0] for API-key providers,
	// no auth for local/no-auth providers.
	Endpoints []EndpointAuth
	// KeyPrefix is the documented key prefix ("sk-ant-", "gsk_",
	// "pplx-"). "" when the provider documents no prefix — key
	// formats are unreliable validators and must never be used to
	// reject a key.
	KeyPrefix string
	// KeyFormat is a short note on the key's shape when documented,
	// e.g. "sk-or-v1- + 64 lowercase hex (73 chars total)". "" when
	// undocumented.
	KeyFormat string
	// HasFreeTier is true only when a free tier was verified from the
	// provider's own docs.
	HasFreeTier bool
	// FreeTierNote describes the verified free tier in the provider's
	// own terms (limits as stated, no aggregate headlines). "" when
	// none verified.
	FreeTierNote string
	// KeyCreationURL is where a user creates an API key for this
	// provider (the provider's key-management page, or the signup
	// page when the key is created at signup). "" when unknown —
	// never guessed from the homepage: a key page is only recorded
	// when a reference hint or the provider's own docs name it.
	// Seeded from the reference router reference hints (see keycreation.go);
	// entries verified against the provider's own docs are marked
	// in that file's source notes.
	KeyCreationURL string
	// Models is the provider's static model list: the model IDs the
	// provider is known to serve, as published by the provider or
	// by models.dev. Empty means "no static list known" — models
	// are then discovered only from the live /v1/models probe at
	// add time, never invented. The list is a snapshot: a provider
	// may serve models not listed here (live discovery wins), and
	// a listed model may since have retired (see DeprecatedModels).
	Models []string
	// Verification says how this entry's facts were established.
	Verification VerificationStatus
	// ToSGrey is true for web-cookie / subscription-OAuth providers,
	// and for the keyless no-auth providers of CT-4 (GAP-TRACK entry
	// 403): catalogued for completeness, never recommended as a
	// routing target.
	ToSGrey bool
	// ToSAvoid is the routing-facing ToS-avoid marker, separate from
	// ToSGrey: when true, the router must avoid this provider as a
	// routing target when a safer route exists (see AvoidAsTarget).
	// ToSGrey is the catalog fact (hard refusal at add/load/auth
	// time); ToSAvoid is the soft routing preference, so the two can
	// diverge — a provider can be avoided without being refused.
	// Set on the five CT-4 entry-403 providers, which are ToS-grey.
	ToSAvoid bool
	// ToolCalling says how the provider performs tool/function
	// calling: natively supported, prompt-emulated, or unknown (the
	// reference states nothing). Only ToolCallingSupported counts
	// as tool support — see SupportsTools. "" (unset) means unknown.
	ToolCalling ToolCalling
	// Retired is true when the provider's public inference API is
	// discontinued and the entry must no longer appear in the live
	// catalog (CT-3): All() excludes it, so it leaves the dashboard
	// feed, FreeTierProviders, and add-by-catalog-ID (which resolves
	// through ByID/All). RetiredByID still returns it, so callers can
	// report the retirement reason instead of "unknown provider".
	// mergeVerified preserves this flag: it only overwrites the
	// fields it lists, and Retired is not one of them.
	Retired bool
	// RetiredReason states what happened, in the provider's own terms
	// where published (e.g. an the reference router deprecation sweep note).
	// Required whenever Retired is true — a retired entry with no
	// reason fails Validate.
	RetiredReason string
	// RetiredOn is the provider-published shutdown date (YYYY-MM-DD),
	// or "" when no shutdown date could be verified — never invented.
	// A sweep/observation date is not a shutdown date and is not
	// recorded here; it belongs in RetiredReason if it matters.
	RetiredOn string
	// SourceURL is where this entry's facts came from (official docs
	// URL for verified entries, reference repo for reference ones).
	SourceURL string
	// Notes holds material quirks in one or two lines, e.g. "chat
	// path has NO /v1", "model = deployment name, not model ID",
	// "account ID is in the URL, not the key".
	Notes string
}

// Validate checks the entry's own integrity: identifiers present,
// verification labelled, base URLs parseable, endpoint auth coherent.
// It does NOT verify the facts against the network — that is what
// VerificationStatus is for.
func (p Provider) Validate() error {
	if strings.TrimSpace(p.ID) == "" {
		return fmt.Errorf("catalog: provider with empty ID (name %q)", p.Name)
	}
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("catalog: provider %q has empty name", p.ID)
	}
	switch p.Verification {
	case VerificationDocs, VerificationReference, VerificationUnverified:
	default:
		return fmt.Errorf("catalog: provider %q has unknown verification status %q", p.ID, p.Verification)
	}
	switch p.ToolCalling {
	case "", ToolCallingUnknown, ToolCallingSupported, ToolCallingEmulated:
	default:
		return fmt.Errorf("catalog: provider %q has unknown tool-calling value %q", p.ID, p.ToolCalling)
	}
	for _, u := range p.BaseURLs {
		if err := validateCatalogBaseURL(u); err != nil {
			return fmt.Errorf("catalog: provider %q: %w", p.ID, err)
		}
	}
	if p.KeyCreationURL != "" {
		if err := validateBaseURL(p.KeyCreationURL); err != nil {
			return fmt.Errorf("catalog: provider %q key-creation URL: %w", p.ID, err)
		}
	}
	for _, m := range p.Models {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("catalog: provider %q has an empty static model ID", p.ID)
		}
	}
	if p.Retired && strings.TrimSpace(p.RetiredReason) == "" {
		return fmt.Errorf("catalog: provider %q is marked retired without a reason", p.ID)
	}
	for _, e := range p.Endpoints {
		if strings.TrimSpace(e.Endpoint) == "" {
			return fmt.Errorf("catalog: provider %q has an endpoint with empty name", p.ID)
		}
		if e.BaseURL != "" {
			if err := validateCatalogBaseURL(e.BaseURL); err != nil {
				return fmt.Errorf("catalog: provider %q endpoint %q: %w", p.ID, e.Endpoint, err)
			}
		}
		if (e.AuthKind == AuthExchange || e.AuthKind == AuthOAuth) && e.Exchange == nil {
			return fmt.Errorf("catalog: provider %q endpoint %q: %s auth requires an Exchange description", p.ID, e.Endpoint, e.AuthKind)
		}
		if (e.AuthKind == AuthBearer || e.AuthKind == AuthXAPIKey || e.AuthKind == AuthAPIKey ||
			e.AuthKind == AuthXGoogAPIKey || e.AuthKind == AuthXFireworksAPIKey || e.AuthKind == AuthCustom) &&
			strings.TrimSpace(e.HeaderName) == "" {
			return fmt.Errorf("catalog: provider %q endpoint %q: %s auth requires a header name", p.ID, e.Endpoint, e.AuthKind)
		}
	}
	return nil
}

// Clone returns a deep copy so callers cannot mutate catalog state.
func (p Provider) Clone() Provider {
	c := p
	c.BaseURLs = append([]string(nil), p.BaseURLs...)
	c.WireFormats = append([]WireFormat(nil), p.WireFormats...)
	c.Models = append([]string(nil), p.Models...)
	if len(p.Endpoints) > 0 {
		c.Endpoints = make([]EndpointAuth, len(p.Endpoints))
		for i, e := range p.Endpoints {
			ne := e
			if e.ExtraHeaders != nil {
				ne.ExtraHeaders = make(map[string]string, len(e.ExtraHeaders))
				for k, v := range e.ExtraHeaders {
					ne.ExtraHeaders[k] = v
				}
			}
			if e.Exchange != nil {
				ex := *e.Exchange
				ne.Exchange = &ex
			}
			c.Endpoints[i] = ne
		}
	}
	return c
}

// DefaultAuthKind is the fallback auth for a provider with no Endpoints
// table: Bearer for API-key/free providers, none for local/no-auth ones,
// OAuth for OAuth providers, cookie for web-cookie ones.
func (p Provider) DefaultAuthKind() AuthKind {
	switch p.Category {
	case CategoryLocal, CategoryNoAuth:
		return AuthNone
	case CategoryOAuth:
		return AuthOAuth
	case CategoryWebCookie:
		return AuthCookie
	default:
		return AuthBearer
	}
}

// DefaultWireFormat returns the provider's first listed wire format, or
// WireUnknown when none is recorded.
func (p Provider) DefaultWireFormat() WireFormat {
	if len(p.WireFormats) == 0 {
		return WireUnknown
	}
	return p.WireFormats[0]
}

// SupportsTools reports whether the provider natively supports tool /
// function calling. Only ToolCallingSupported counts: emulated tool
// calling is prompt-parsed text, not native support, and unknown (or
// unset) means the reference states nothing — neither ever satisfies
// a tool-capable check (CT-4 / GAP-TRACK entry 403: an emulated
// entry must never be picked by an auto pool as tool-capable).
func (p Provider) SupportsTools() bool {
	return p.ToolCalling == ToolCallingSupported
}

// AvoidAsTarget reports whether the provider should be avoided as a
// routing target when a safer route exists. It wires the dedicated
// ToSAvoid marker together with ToSGrey: either signal avoids the
// target. ToSGrey providers are additionally refused outright at
// add/load/auth time (internal/provider/registry.go); this is the
// soft routing preference for targets that are addable.
func (p Provider) AvoidAsTarget() bool {
	return p.ToSAvoid || p.ToSGrey
}

// Endpoint returns the EndpointAuth for the named surface and variant
// ("default" when variant is ""), or false when the provider records no
// such surface.
func (p Provider) Endpoint(name, variant string) (EndpointAuth, bool) {
	for _, e := range p.Endpoints {
		if e.Endpoint == name && e.Variant == variant {
			return e, true
		}
	}
	return EndpointAuth{}, false
}

// All returns the live catalog: reference entries, with verified entries
// overlaid by ID, plus additional providers — excluding retired
// providers (CT-3) and system entries (reference-internal loopback
// providers), neither of which is a real upstream. The result is a
// fresh slice of copies on every call.
//
// Order is deterministic: reference order, then verified-only additions
// in file order, then additional providers in file order.
func All() []Provider {
	all := allIncludingRetired()
	out := make([]Provider, 0, len(all))
	for _, p := range all {
		if p.Retired {
			continue
		}
		out = append(out, p)
	}
	return out
}

// RetiredProviders returns every retired entry, in catalog order.
// These are the providers All() excludes; use RetiredByID to explain
// a refusal instead of reporting "unknown provider".
func RetiredProviders() []Provider {
	var out []Provider
	for _, p := range allIncludingRetired() {
		if p.Retired {
			out = append(out, p)
		}
	}
	return out
}

// RetiredByID returns the retired entry with the given ID or alias, or
// false when the ID names no retired provider. It matches aliases too
// (e.g. "bb" for blackbox) so an add-by-alias attempt gets the same
// retirement reason as an add-by-ID attempt.
func RetiredByID(idOrAlias string) (Provider, bool) {
	for _, p := range RetiredProviders() {
		if p.ID == idOrAlias || (p.Alias != "" && p.Alias == idOrAlias) {
			return p, true
		}
	}
	return Provider{}, false
}

// IsRetired reports whether the given ID or alias names a retired provider.
func IsRetired(idOrAlias string) bool {
	_, ok := RetiredByID(idOrAlias)
	return ok
}

// allIncludingRetired builds the full catalog — reference entries, with
// verified entries overlaid by ID, plus additional providers — before
// the retired exclusion. All() filters its result; RetiredProviders
// and RetiredByID read from it so the retirement reason stays
// available after the entry leaves the live surface.
func allIncludingRetired() []Provider {
	byID := make(map[string]int, len(referenceProviders))
	out := make([]Provider, 0, len(referenceProviders)+len(verifiedProviders)+len(additionalProviders))
	for _, p := range referenceProviders {
		if p.Category == CategorySystem {
			continue
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range verifiedProviders {
		if i, ok := byID[p.ID]; ok {
			// G7 (parity tracker Area 3, WRONG row): the overlay MERGES,
			// it does not replace. The old code did
			// `out[i] = p.Clone()`, which dropped every
			// reference-only fact the verified entry did not
			// restate — HasFreeTier for 10 providers
			// (cerebras, cloudflare-ai, deepseek, fireworks,
			// gemini, groq, mistral, nvidia, ollama-cloud,
			// openrouter, vertex). A verified entry that does
			// not mention a free tier means "not checked in
			// that pass", never "no free tier", so the
			// reference flag survives. See mergeVerified.
			out[i] = mergeVerified(out[i], p)
			continue
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	// Triage fixes (2026-11-18 catalog triage, CHECKLIST 253):
	// base-URL fills and display-name renames on reference entries,
	// plus reference-only new providers. See providers_triage.go.
	out = applyTriageFixes(out, byID)
	for _, p := range additionalProviders {
		if _, ok := byID[p.ID]; ok {
			continue // additional must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	// Key-creation links (G7, parity tracker Area 3): fill KeyCreationURL
	// from the seed map where the entry records none. The fill
	// runs last so it covers reference, verified-only, triage and
	// additional entries alike; it never overwrites a URL the
	// entry already carries.
	for i := range out {
		if out[i].KeyCreationURL == "" {
			if u, ok := keyCreationURLs[out[i].ID]; ok {
				out[i].KeyCreationURL = u
			}
		}
	}
	return out
}

// mergeVerified overlays a docs-verified entry onto the reference
// entry it matches. Verified facts win wherever the verified entry
// states them; reference-only facts survive wherever it does not.
//
//   - Verified wins when set: Name, Website, Category, BaseURLs,
//     WireFormats, Endpoints, KeyPrefix, KeyFormat, Notes,
//     SourceURL, and the Verification label itself (the merged
//     entry is verified-from-docs).
//   - Reference survives when the verified entry is silent: Alias,
//     KeyPrefix/KeyFormat (reference entries carry none today, but
//     the rule must not depend on that), and the static Models
//     list.
//   - Flags merge by OR, never by overwrite: HasFreeTier,
//     ToSGrey and ToSAvoid. A verified entry that does not mention
//     a free tier means "not checked in that pass", never "no free
//     tier"; ToSGrey and ToSAvoid are safety flags and must never
//     be dropped by an overlay. FreeTierNote and KeyCreationURL
//     take the verified value when set, else the reference value.
//
// The result is a fresh copy; neither input is mutated.
func mergeVerified(ref, ver Provider) Provider {
	merged := ref.Clone()
	if ver.Name != "" {
		merged.Name = ver.Name
	}
	if ver.Website != "" {
		merged.Website = ver.Website
	}
	if ver.Category != "" {
		merged.Category = ver.Category
	}
	if len(ver.BaseURLs) > 0 {
		merged.BaseURLs = append([]string(nil), ver.BaseURLs...)
	}
	if len(ver.WireFormats) > 0 {
		merged.WireFormats = append([]WireFormat(nil), ver.WireFormats...)
	}
	if len(ver.Endpoints) > 0 {
		merged.Endpoints = ver.Clone().Endpoints
	}
	if ver.Alias != "" {
		merged.Alias = ver.Alias
	}
	if ver.KeyPrefix != "" {
		merged.KeyPrefix = ver.KeyPrefix
	}
	if ver.KeyFormat != "" {
		merged.KeyFormat = ver.KeyFormat
	}
	if ver.Notes != "" {
		merged.Notes = ver.Notes
	}
	if ver.SourceURL != "" {
		merged.SourceURL = ver.SourceURL
	}
	if ver.FreeTierNote != "" {
		merged.FreeTierNote = ver.FreeTierNote
	}
	if ver.KeyCreationURL != "" {
		merged.KeyCreationURL = ver.KeyCreationURL
	}
	if len(ver.Models) > 0 {
		merged.Models = append([]string(nil), ver.Models...)
	}
	merged.HasFreeTier = ref.HasFreeTier || ver.HasFreeTier
	merged.ToSGrey = ref.ToSGrey || ver.ToSGrey
	merged.ToSAvoid = ref.ToSAvoid || ver.ToSAvoid
	merged.Verification = ver.Verification
	return merged
}

// ByID returns the catalog entry with the given ID, or false when absent.
func ByID(id string) (Provider, bool) {
	for _, p := range All() {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// ByAlias returns the entry whose ID or Alias matches, preferring ID.
func ByAlias(aliasOrID string) (Provider, bool) {
	if p, ok := ByID(aliasOrID); ok {
		return p, true
	}
	for _, p := range All() {
		if p.Alias != "" && p.Alias == aliasOrID {
			return p, true
		}
	}
	return Provider{}, false
}

// Count returns the number of entries in the full catalog.
func Count() int { return len(All()) }

// CountByVerification returns how many entries carry each verification
// status. The honest headline: verified vs reference-only vs unverified.
func CountByVerification() map[VerificationStatus]int {
	counts := map[VerificationStatus]int{
		VerificationDocs:       0,
		VerificationReference:  0,
		VerificationUnverified: 0,
	}
	for _, p := range All() {
		counts[p.Verification]++
	}
	return counts
}

// CountByCategory returns how many entries fall in each category.
func CountByCategory() map[Category]int {
	counts := map[Category]int{}
	for _, p := range All() {
		counts[p.Category]++
	}
	return counts
}

// ByCategory returns all entries in the given category, in catalog order.
func ByCategory(c Category) []Provider {
	var out []Provider
	for _, p := range All() {
		if p.Category == c {
			out = append(out, p)
		}
	}
	return out
}

// Routable reports whether the provider has at least one concrete
// (non-template) http(s) base URL — i.e. the router can actually
// send it a request once a key is supplied. Templated bases
// (Azure {resource}, Vertex {LOCATION}) are NOT routable as-is:
// the operator must substitute the placeholder first. A loopback
// base (localhost) IS routable: local runtimes serve on it.
func (p Provider) Routable() bool {
	for _, u := range p.BaseURLs {
		if strings.Contains(u, "{") || strings.Contains(u, "}") {
			continue
		}
		parsed, err := url.Parse(strings.TrimSpace(u))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			continue
		}
		return true
	}
	return false
}

// StaticModels returns the provider's static model list (a copy),
// or nil when no static list is known. Live /v1/models discovery
// at add time is the fallback; this list never invents a model.
func (p Provider) StaticModels() []string {
	return append([]string(nil), p.Models...)
}

// BaseURLCoverage is the honest accounting of routable-base-URL
// coverage (parity tracker Area 3, "Providers with a routable base URL"):
// how many entries can actually be added, how many cannot, and
// where the un-addable ones sit. Categories with no API surface
// by nature (web-cookie, OAuth, local, no-auth, cloud-agent,
// upstream-proxy) are counted separately from the real gap:
// API-capable providers (api-key, audio, search) with no base URL.
type BaseURLCoverage struct {
	Total int
	// WithBaseURL counts entries with any BaseURLs entry at all.
	WithBaseURL int
	// Routable counts entries with a concrete (non-template) base.
	Routable int
	// TemplateOnly counts entries whose only bases are templates.
	TemplateOnly int
	// WithoutByCategory counts entries with no BaseURLs at all.
	WithoutByCategory map[Category]int
	// APICapableWithout counts api-key/audio/search entries with no
	// base URL — the providers a user cannot add even though an
	// API exists to add them to.
	APICapableWithout int
}

// BaseURLCoverageOf measures coverage over the given entries.
func BaseURLCoverageOf(all []Provider) BaseURLCoverage {
	cov := BaseURLCoverage{Total: len(all), WithoutByCategory: map[Category]int{}}
	for _, p := range all {
		switch {
		case len(p.BaseURLs) == 0:
			cov.WithoutByCategory[p.Category]++
			switch p.Category {
			case CategoryAPIKey, CategoryAudio, CategorySearch:
				cov.APICapableWithout++
			}
		case p.Routable():
			cov.WithBaseURL++
			cov.Routable++
		default:
			cov.WithBaseURL++
			cov.TemplateOnly++
		}
	}
	return cov
}

// MeasureBaseURLCoverage measures coverage over the full catalog.
func MeasureBaseURLCoverage() BaseURLCoverage { return BaseURLCoverageOf(All()) }

// FreeTierProviders returns every entry with a verified free tier,
// in catalog order. Retired providers are excluded: All() drops
// them, so their stale free-tier flags can no longer inflate the
// count (CT-3).
func FreeTierProviders() []Provider {
	var out []Provider
	for _, p := range All() {
		if p.HasFreeTier {
			out = append(out, p)
		}
	}
	return out
}

// FreeTierCount returns how many entries carry a verified free tier.
func FreeTierCount() int { return len(FreeTierProviders()) }

// FreeTierPool is one free-tier pool: the free-tier providers in
// a single category, with the docs-verified share. Pools are
// groupings of the verified free-tier set — the reference router's pools are
// budget pools, and no verified per-provider free budget exists
// anywhere in this catalog to build budget pools from (inventing
// one would breach the honesty rules), so the grouping is by
// category and the summary reports counts, never invented
// token budgets.
type FreeTierPool struct {
	// Name is the pool name ("api-key", "local", ...).
	Name string
	// Providers are the pool's free-tier entries, catalog order.
	Providers []Provider
	// VerifiedDocs counts pool members verified from docs.
	VerifiedDocs int
}

// FreeTierPools groups the free-tier providers into per-category
// pools, in first-appearance order.
func FreeTierPools() []FreeTierPool {
	var pools []FreeTierPool
	byName := map[string]int{}
	for _, p := range FreeTierProviders() {
		name := string(p.Category)
		i, ok := byName[name]
		if !ok {
			i = len(pools)
			byName[name] = i
			pools = append(pools, FreeTierPool{Name: name})
		}
		pools[i].Providers = append(pools[i].Providers, p)
		if p.Verification == VerificationDocs {
			pools[i].VerifiedDocs++
		}
	}
	return pools
}

// FreeTierSummary is the honest headline for the free tier: counts
// only, never an invented aggregate token budget.
type FreeTierSummary struct {
	// Total free-tier providers.
	Total int
	// VerifiedDocs counts free-tier providers verified from docs.
	VerifiedDocs int
	// WithNote counts free-tier providers carrying a FreeTierNote.
	WithNote int
	// Pools counts free-tier pools (see FreeTierPools).
	Pools int
}

// SummarizeFreeTier summarises the free tier over the full catalog.
func SummarizeFreeTier() FreeTierSummary {
	var s FreeTierSummary
	for _, p := range FreeTierProviders() {
		s.Total++
		if p.Verification == VerificationDocs {
			s.VerifiedDocs++
		}
		if p.FreeTierNote != "" {
			s.WithNote++
		}
	}
	s.Pools = len(FreeTierPools())
	return s
}

// FreeTierRanking returns the free-tier providers in ranking order:
// docs-verified first (their free tier was checked against the
// provider's own docs), then entries with a free-tier note, then
// the rest; ties break by ID so the order is deterministic. The
// ranking is a display order for the free-tier list, not a quality
// score — it ranks evidence strength, never invents one.
func FreeTierRanking() []Provider {
	ranked := FreeTierProviders()
	score := func(p Provider) int {
		s := 0
		if p.Verification == VerificationDocs {
			s += 2
		}
		if p.FreeTierNote != "" {
			s += 1
		}
		return s
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if si, sj := score(ranked[i]), score(ranked[j]); si != sj {
			return si > sj
		}
		return ranked[i].ID < ranked[j].ID
	})
	return ranked
}

// KeyCreationURLFor returns the key-creation URL recorded for id,
// or false when the catalog records none for it.
func KeyCreationURLFor(id string) (string, bool) {
	p, ok := ByID(id)
	if !ok || p.KeyCreationURL == "" {
		return "", false
	}
	return p.KeyCreationURL, true
}

// ByAuthKind returns all entries whose default auth kind is k (the
// auth of their default endpoint, or DefaultAuthKind when no endpoints
// are recorded).
func ByAuthKind(k AuthKind) []Provider {
	var out []Provider
	for _, p := range All() {
		if p.DefaultEndpointAuth().AuthKind == k {
			out = append(out, p)
		}
	}
	return out
}

// Search returns entries whose ID, name, or alias contains query
// (case-insensitive substring). An empty query returns all entries.
func Search(query string) []Provider {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return All()
	}
	var out []Provider
	for _, p := range All() {
		if strings.Contains(strings.ToLower(p.ID), q) ||
			strings.Contains(strings.ToLower(p.Name), q) ||
			(p.Alias != "" && strings.Contains(strings.ToLower(p.Alias), q)) {
			out = append(out, p)
		}
	}
	return out
}

// DefaultEndpointAuth returns the auth table entry for the provider's
// default surface (its first Endpoint, or a synthesized default when
// the provider records no endpoints). It never invents a base URL: the
// synthesized default uses BaseURLs[0] when present, "" otherwise.
func (p Provider) DefaultEndpointAuth() EndpointAuth {
	if len(p.Endpoints) > 0 {
		return p.Endpoints[0]
	}
	base := ""
	if len(p.BaseURLs) > 0 {
		base = p.BaseURLs[0]
	}
	header := ""
	switch p.DefaultAuthKind() {
	case AuthBearer:
		header = "Authorization"
	case AuthXAPIKey:
		header = "x-api-key"
	case AuthAPIKey:
		header = "api-key"
	case AuthXGoogAPIKey:
		header = "x-goog-api-key"
	case AuthXFireworksAPIKey:
		header = "X-Fireworks-Api-Key"
	}
	return EndpointAuth{
		Endpoint:   EndpointChat,
		BaseURL:    base,
		AuthKind:   p.DefaultAuthKind(),
		HeaderName: header,
	}
}

// ValidateAll checks every catalog entry's integrity and that no two
// entries share an ID. It returns the first problem found.
func ValidateAll() error {
	seen := make(map[string]bool, len(All()))
	for _, p := range All() {
		if seen[p.ID] {
			return fmt.Errorf("catalog: duplicate provider ID %q", p.ID)
		}
		seen[p.ID] = true
		if err := p.Validate(); err != nil {
			return err
		}
	}
	return nil
}
