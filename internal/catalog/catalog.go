// Package catalog is the Codvian Router provider catalog: the list of
// LLM providers the router knows about, how to authenticate to each one,
// and which wire format each one speaks.
//
// The catalog is deliberately bigger than any single competitor's list:
// it starts from the providers OmniRoute documents (394 entries, taken
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

	// VerificationReference means the entry was taken from OmniRoute
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
	// Verification says how this entry's facts were established.
	Verification VerificationStatus
	// ToSGrey is true for web-cookie / subscription-OAuth providers:
	// catalogued for completeness, never recommended as a routing
	// target.
	ToSGrey bool
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
	for _, u := range p.BaseURLs {
		if err := validateCatalogBaseURL(u); err != nil {
			return fmt.Errorf("catalog: provider %q: %w", p.ID, err)
		}
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

// All returns the full catalog: reference entries, with verified entries
// overlaid by ID, plus additional providers. The result is a fresh slice
// of copies on every call. System entries (OmniRoute-internal loopback
// providers) are excluded: they are not real upstreams.
//
// Order is deterministic: reference order, then verified-only additions
// in file order, then additional providers in file order.
func All() []Provider {
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
			out[i] = p.Clone() // verified entry replaces the reference one
			continue
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	for _, p := range additionalProviders {
		if _, ok := byID[p.ID]; ok {
			continue // additional must never shadow an existing ID
		}
		byID[p.ID] = len(out)
		out = append(out, p.Clone())
	}
	return out
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
