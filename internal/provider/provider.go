// Package provider implements the Codvian Router provider registry and an
// OpenAI-compatible upstream client.
//
// A Provider is any LLM backend reachable over HTTP: Codvian is just one
// option (see CodvianPreset in registry.go) alongside OpenAI, Anthropic
// via a compatible gateway, Ollama, or any other OpenAI-compatible endpoint.
// API keys are stored encrypted at rest (AES-256-GCM via internal/crypto);
// see the KeyCipher in registry.go.
//
// This package has no dependency on internal/config, so it compiles
// standalone; it does import internal/crypto, which is stdlib-only
// (crypto/aes, crypto/cipher).
package provider

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// WireFormatOpenAI is the default wire format: OpenAI-compatible
// /v1/models and /v1/chat/completions.
const WireFormatOpenAI = "openai"

// WireFormatAnthropic is the Anthropic Messages wire format
// (POST /v1/messages), spoken by internal/wire. A provider whose
// catalog wire format resolves to this speaks Messages, not OpenAI
// chat completions; see NewClient (client.go).
const WireFormatAnthropic = "anthropic"

// WireFormatGemini is the Gemini generateContent wire format
// (POST /v1beta/models/{model}:generateContent), spoken by
// internal/wire. A provider whose catalog wire format resolves to
// this speaks generateContent, not OpenAI chat completions; see
// NewClient (client.go).
const WireFormatGemini = "gemini"

// WireFormatCohere is the Cohere Chat API v2 wire format
// (POST /v2/chat), spoken by internal/wire. A provider whose catalog
// wire format resolves to this speaks Cohere v2 chat, not OpenAI
// chat completions; see NewClient (client.go).
const WireFormatCohere = "cohere"

// WireFormatBedrock is the Bedrock Converse wire format
// (POST /model/{modelId}/converse), spoken by internal/wire and
// signed with AWS SigV4 (see sigv4.go). A provider whose catalog
// wire format resolves to this speaks Converse, not OpenAI chat
// completions; see NewClient (client.go).
const WireFormatBedrock = "bedrock-converse"

// WireFormatVertex is the Vertex AI (Gemini) wire format: the
// Gemini generateContent shape served from Vertex AI
// (POST {location}-aiplatform.googleapis.com/v1/projects/{project}/
// locations/{location}/publishers/google/models/{model}
// :generateContent), authenticated by a minted OAuth2 access token
// from the operator's service-account JSON (see vertex_client.go).
// It is deliberately distinct from WireFormatGemini (AI Studio,
// x-goog-api-key): the two surfaces share the request/response
// shape but not the auth exchange, the base URL, or the model
// path, and conflating them would send a Vertex request to the
// wrong host with the wrong credential.
const WireFormatVertex = "vertex"

// WireFormatUnknown means auto-detection could not confirm an
// OpenAI-compatible endpoint. The provider is still stored, but routing
// to it should be treated as best-effort.
const WireFormatUnknown = "unknown"

// QuotaUnknown is the QuotaHint value meaning "remaining quota not known".
const QuotaUnknown = -1.0

// Model is a single model offered by a provider. ContextWindow and prices
// are best-effort metadata: 0 means unknown (most upstreams do not report
// pricing via /v1/models, and it is filled in later if known).
// Deprecated marks a model the provider has retired: /v1/models hides it
// by default (with ?include=deprecated to list it again) and the
// dashboard shows it as deprecated — but requests naming it are still
// forwarded, never hard-blocked on a possibly stale catalog entry.
// The flag is operator-settable in the stored provider; the catalog's
// own retired-model table (catalog.IsDeprecated) marks the same models
// even when the flag was never set.
//
// Region is the operator-declared serving region for this model
// (CHECKLIST 257, research-log entry 232): e.g. "us-east-1" or
// "eu-west-1". It is a first-class routing dimension — failover
// tries the same model in another region before switching models
// — and it is NEVER guessed: "" means unknown, never a default
// region, and no region is ever inferred from a base URL, a
// provider name, or a catalog entry. A model-level region wins
// over the provider-level one (see Provider.RegionFor); a model
// with no region inherits its provider's, and a provider with
// none leaves the model region unknown.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"context_window,omitempty"`
	// Vision reports whether the model accepts image input
	// (G19 Area 1, PARITY rows "auto/best-*" vision): set from
	// the models.dev [modalities] table by the opt-in sync
	// (ApplySyncedVision), or by the operator in the model
	// entry. False means "not known vision-capable", never a
	// claim of text-only — a model the sync never reported
	// modalities for stays false, and the auto-variant vision
	// filter fails open on it (full pool, noted in the
	// resolution). Never guessed from the model id.
	Vision bool `json:"vision,omitempty"`
	// VisionKnown reports whether the vision capability above
	// was affirmatively reported by a source (B3-24 modality
	// bridges): true only when the models.dev sync saw a
	// [modalities] table for this model (ApplySyncedVision with
	// known=true). False means "never reported" — the Vision
	// flag then carries no claim either way, and the bridge
	// fails open on it (never reroutes away from an unknown
	// model). An operator-set Vision flag does NOT set this:
	// the config decode path has no reporting provenance, so
	// an operator-set flag is treated as unknown to the
	// bridge, exactly like an unsynced one.
	VisionKnown bool `json:"vision_known,omitempty"`
	// Tools reports whether the model accepts tool/function calls
	// (CT-1, GAP-TRACK): tri-state — nil means UNKNOWN, never a
	// guess from the model id. A model the live probe / models.dev
	// sync never reported stays nil; non-nil true means confirmed
	// tool-capable, non-nil false means confirmed incapable. Set
	// from the live probe / models.dev where available, or by the
	// operator in the model entry. The direct-path pre-check
	// (internal/router) skips only confirmed-incapable models
	// and fails open on unknown — an unknown model is tried,
	// never assumed incapable.
	Tools          *bool   `json:"tools,omitempty"`
	InputPriceUSD  float64 `json:"input_price_usd,omitempty"`
	OutputPriceUSD float64 `json:"output_price_usd,omitempty"`
	Deprecated     bool    `json:"deprecated,omitempty"`
	Region         string  `json:"region,omitempty"`
	// FixedParams pins request parameters for this deployment
	// (CHECKLIST 245, LiteLLM #51254): admin-set values that
	// outrank anything the client sends in the request body.
	// A nil entry means "not pinned": the client's value (or
	// the provider default) stands. A set entry always wins —
	// the client's conflicting value is replaced, never
	// silently honoured — and the replacement is signalled on
	// the response (X-Codvian-Deployment-Override), never
	// silent. Pins live on the model because a model entry is
	// this router's deployment: one provider + one model id is
	// the unit the admin configures and the client addresses.
	FixedParams *ModelFixedParams `json:"fixed_params,omitempty"`
}

// ToolsCapable reports the model's tool capability (CT-1):
// (capable, known). known is false when Tools is nil —
// unknown, never a false-positive capable and never a
// false-positive incapable. known && capable means confirmed
// tool-capable; known && !capable means confirmed incapable.
func (m Model) ToolsCapable() (capable, known bool) {
	if m.Tools == nil {
		return false, false
	}
	return *m.Tools, true
}

// ModelFixedParams is the admin-set per-deployment parameter pin
// for one model. Every field is a pointer so "pinned" stays
// distinct from "unset": a nil Temperature means the client may
// choose it; a non-nil one is the value the request is sent with,
// whatever the client asked for. Ranges are validated at config
// load (temperature [0, 2], top_p (0, 1], max_tokens >= 1).
type ModelFixedParams struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
}

// Clone deep-copies a ModelFixedParams: the struct copy alone
// would share the inner value pointers, so a clone that moved a
// pin would move the original's too.
func (p *ModelFixedParams) Clone() *ModelFixedParams {
	if p == nil {
		return nil
	}
	out := *p
	if p.Temperature != nil {
		v := *p.Temperature
		out.Temperature = &v
	}
	if p.TopP != nil {
		v := *p.TopP
		out.TopP = &v
	}
	if p.MaxTokens != nil {
		v := *p.MaxTokens
		out.MaxTokens = &v
	}
	return &out
}

// req: every pinned value replaces the client's value for that
// field. It returns the names of the pinned fields it applied
// (in temperature, top_p, max_tokens order); an empty return
// means the deployment pins nothing and the request is
// untouched. A pinned value is applied even when the client sent
// nothing for that field — the pin is the deployment's value,
// not just a ceiling on the client's.
func ApplyFixedParams(req *ChatRequest, m Model) []string {
	if req == nil || m.FixedParams == nil {
		return nil
	}
	var applied []string
	if m.FixedParams.Temperature != nil {
		req.Temperature = *m.FixedParams.Temperature
		applied = append(applied, "temperature")
	}
	if m.FixedParams.TopP != nil {
		req.TopP = *m.FixedParams.TopP
		applied = append(applied, "top_p")
	}
	if m.FixedParams.MaxTokens != nil {
		req.MaxTokens = *m.FixedParams.MaxTokens
		applied = append(applied, "max_tokens")
	}
	return applied
}

// Provider is one upstream LLM backend.
//
// CatalogID links the provider to an entry in internal/catalog
// ("groq", "fireworks", ...). It is empty for fully-custom providers
// (any base URL + key, added by hand). When set, the base URL, wire
// format and auth scheme come from the catalog entry (BaseURL may
// still be an operator override, e.g. a proxy or a mock in tests),
// and the registry resolves credentials per (provider, endpoint)
// through catalog.ResolveAuth instead of assuming Bearer.
// Verification is the catalog entry's verification status
// ("verified-from-docs", "from-reference-metadata-only",
// "unverified") — surfaced to the operator, never hidden.
// AuthEndpoint/AuthVariant name the catalog surface the stored
// credentials are for ("" = the entry's default surface/variant).
type Provider struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	BaseURL         string  `json:"base_url"`
	APIKeyEncrypted string  `json:"api_key_encrypted"`
	WireFormat      string  `json:"wire_format"`
	Models          []Model `json:"models,omitempty"`
	// ModelsFetchedAt is when the stored model list was last
	// fetched live from the upstream (CT-7): stamped at add time
	// (the add-time ListModels call) and on every successful
	// refresh (manual or scheduled — see RefreshModels). Zero
	// means never fetched: a provider loaded from a config
	// written before this field existed, or one added through
	// the chat-only path whose upstream exposes no model list.
	// A zero value is always stale (see ModelsListStale) — the
	// list's age is unknown, never assumed fresh.
	ModelsFetchedAt time.Time `json:"models_fetched_at,omitempty"`
	Enabled         bool      `json:"enabled"`
	QuotaHint       float64   `json:"quota_hint"`
	CreatedAt       time.Time `json:"created_at"`
	CatalogID       string    `json:"catalog_id,omitempty"`
	Verification    string    `json:"verification,omitempty"`
	AuthEndpoint    string    `json:"auth_endpoint,omitempty"`
	AuthVariant     string    `json:"auth_variant,omitempty"`
	// NoTrainingData — see ProviderConfig: the operator's
	// assertion that this provider does not use request data
	// for training; what routing.data_collection=deny trusts.
	NoTrainingData bool `json:"no_training_data,omitempty"`
	// Region is the operator-declared serving region for this
	// provider (CHECKLIST 257): e.g. "us-east-1". It is the
	// fallback for models that declare no region of their own
	// (see RegionFor). "" means unknown — never a default, never
	// inferred from the base URL or the provider's name. The
	// catalog carries no region for any provider, deliberately:
	// a region is an operator fact about THEIR deployment, not
	// a property of the vendor.
	Region string `json:"region,omitempty"`
	// Keys is the provider's API-key pool (CHECKLIST 175): one
	// entry per key, in pool order — preferred keys first, then
	// reserve keys (see internal/provider/keys.go). Empty for
	// providers stored before pools existed: EffectiveKeys
	// synthesizes a single preferred key from APIKeyEncrypted,
	// so a legacy provider keeps routing exactly as before.
	Keys []ProviderKey `json:"keys,omitempty"`
}

// ProviderConfig is the minimal persisted shape of a Provider.
//
// CONTRACT NOTE (2026-11-17): internal/config/config.go aliases this type
// (type ProviderConfig = provider.ProviderConfig), so the config file and
// the provider registry can never drift apart (same field names and JSON
// tags, same Models element type). The Registry persists through its
// Store interface (see registry.go); a config-backed Store can replace
// JSONFileStore without touching this type.
type ProviderConfig struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	BaseURL         string  `json:"base_url"`
	APIKeyEncrypted string  `json:"api_key_encrypted"`
	WireFormat      string  `json:"wire_format"`
	Models          []Model `json:"models,omitempty"`
	// ModelsFetchedAt — see Provider: when the stored model
	// list was last fetched live. Zero = never fetched.
	ModelsFetchedAt time.Time `json:"models_fetched_at,omitempty"`
	Enabled         bool      `json:"enabled"`
	QuotaHint       float64   `json:"quota_hint"`
	CreatedAt       time.Time `json:"created_at"`
	CatalogID       string    `json:"catalog_id,omitempty"`
	Verification    string    `json:"verification,omitempty"`
	AuthEndpoint    string    `json:"auth_endpoint,omitempty"`
	AuthVariant     string    `json:"auth_variant,omitempty"`
	// NoTrainingData is the operator's assertion that this
	// provider does not use request data for training
	// (CHECKLIST 157): it is what routing.data_collection=deny
	// trusts. It is an operator assertion, not a catalog fact —
	// the catalog ships no vendor training-policy database, so
	// nothing here is inferred from a provider's name or docs.
	// Absent (false) means "not verified", and "deny" excludes
	// the provider rather than guessing it safe. Local providers
	// (catalog category local, or a loopback base URL) are safe
	// by construction whether or not this is set: the data never
	// leaves the machine.
	NoTrainingData bool `json:"no_training_data,omitempty"`
	// Region is the operator-declared serving region for this
	// provider (CHECKLIST 257) — the fallback for models that
	// declare no region of their own. "" means unknown, never
	// a default and never inferred.
	Region string `json:"region,omitempty"`
	// Keys is the provider's API-key pool (CHECKLIST 175).
	// Empty for providers stored before pools existed — see
	// Provider.EffectiveKeys.
	Keys []ProviderKey `json:"keys,omitempty"`
}

// ToConfig converts a Provider to its persisted form.
func (p *Provider) ToConfig() ProviderConfig {
	if p == nil {
		return ProviderConfig{}
	}
	models := make([]Model, len(p.Models))
	copy(models, p.Models)
	keys := make([]ProviderKey, len(p.Keys))
	copy(keys, p.Keys)
	return ProviderConfig{
		ID:              p.ID,
		Name:            p.Name,
		BaseURL:         p.BaseURL,
		APIKeyEncrypted: p.APIKeyEncrypted,
		WireFormat:      p.WireFormat,
		Models:          models,
		ModelsFetchedAt: p.ModelsFetchedAt,
		Enabled:         p.Enabled,
		QuotaHint:       p.QuotaHint,
		CreatedAt:       p.CreatedAt,
		CatalogID:       p.CatalogID,
		Verification:    p.Verification,
		AuthEndpoint:    p.AuthEndpoint,
		AuthVariant:     p.AuthVariant,
		NoTrainingData:  p.NoTrainingData,
		Region:          p.Region,
		Keys:            keys,
	}
}

// ProviderFromConfig restores a Provider from its persisted form, applying
// defaults for fields an older config may lack.
func ProviderFromConfig(c ProviderConfig) *Provider {
	if c.WireFormat == "" {
		c.WireFormat = WireFormatOpenAI
	}
	models := make([]Model, len(c.Models))
	copy(models, c.Models)
	keys := make([]ProviderKey, len(c.Keys))
	copy(keys, c.Keys)
	return &Provider{
		ID:              c.ID,
		Name:            c.Name,
		BaseURL:         NormalizeBaseURL(c.BaseURL),
		APIKeyEncrypted: c.APIKeyEncrypted,
		WireFormat:      c.WireFormat,
		Models:          models,
		ModelsFetchedAt: c.ModelsFetchedAt,
		Enabled:         c.Enabled,
		QuotaHint:       c.QuotaHint,
		CreatedAt:       c.CreatedAt,
		CatalogID:       c.CatalogID,
		Verification:    c.Verification,
		AuthEndpoint:    c.AuthEndpoint,
		AuthVariant:     c.AuthVariant,
		NoTrainingData:  c.NoTrainingData,
		Region:          c.Region,
		Keys:            keys,
	}
}

// EffectiveRegion resolves the serving region for one model on
// one provider (CHECKLIST 257): the model's own declared region
// wins; otherwise the provider's declared region; otherwise ""
// (unknown). A region is never guessed — an empty result means
// the operator declared nothing, and callers must treat it as
// unknown, never as a default region.
func EffectiveRegion(providerRegion, modelRegion string) string {
	if strings.TrimSpace(modelRegion) != "" {
		return strings.TrimSpace(modelRegion)
	}
	return strings.TrimSpace(providerRegion)
}

// RegionFor returns the effective serving region for model on
// this provider (see EffectiveRegion): the model's declared
// region, else the provider's, else "" (unknown). A model the
// provider does not list falls back to the provider's region —
// the provider's deployments share a region unless a model says
// otherwise.
func (p *Provider) RegionFor(model string) string {
	if p == nil {
		return ""
	}
	for _, m := range p.Models {
		if m.ID == model {
			return EffectiveRegion(p.Region, m.Region)
		}
	}
	return strings.TrimSpace(p.Region)
}

// ValidateRegion reports whether a declared region string is
// well-formed: "" (unknown) is always valid; otherwise the value
// must be a single token — no whitespace, no control characters
// (a region is an identifier like "us-east-1", never a sentence).
// The check is shape-only: the router never verifies a region
// against a vendor's region list, because it must not guess
// which regions a provider offers.
func ValidateRegion(region string) error {
	if region == "" {
		return nil
	}
	for _, r := range region {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("region %q must be a single token without whitespace", region)
		}
	}
	return nil
}

// Clone returns a deep copy of the provider (models slice included) so
// callers cannot mutate registry state through a returned pointer.
func (p *Provider) Clone() *Provider {
	if p == nil {
		return nil
	}
	c := *p
	c.Models = make([]Model, len(p.Models))
	copy(c.Models, p.Models)
	c.Keys = make([]ProviderKey, len(p.Keys))
	copy(c.Keys, p.Keys)
	return &c
}

// ModelsListStale reports whether the stored model list is
// older than ModelListRefreshInterval at now (CT-7): a list
// fetched longer ago than the scheduled refresh cadence may be
// missing new models or still naming delisted ones. A provider
// whose list was never fetched (zero ModelsFetchedAt) is always
// stale — unknown age is never treated as fresh. A fetched-at in
// the future (clock skew) is not stale.
func (p *Provider) ModelsListStale(now time.Time) bool {
	if p == nil || p.ModelsFetchedAt.IsZero() {
		return true
	}
	return !now.Before(p.ModelsFetchedAt.Add(ModelListRefreshInterval))
}

// NewProviderID returns a fresh provider ID ("prov_" + 16 hex chars from
// crypto/rand).
func NewProviderID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially impossible on a usable system;
		// fall back to a time-based value rather than returning an empty ID.
		return fmt.Sprintf("prov_%d", time.Now().UnixNano())
	}
	return "prov_" + hex.EncodeToString(b[:])
}

// NormalizeBaseURL trims whitespace and trailing slashes and lowercases
// the scheme and host, so base URLs that differ only by a trailing slash
// or by scheme/host case register to the same canonical value. The path
// keeps its case: "/V1" and "/v1" are different paths and are not
// collapsed. It does not add or remove a "/v1" suffix: the client
// appends "/v1/..." when the base path does not already end in "/v1"
// (see joinAPIPath in client.go). An unparseable input, or one with no
// scheme/host, falls back to the plain trim, so ValidateBaseURL still
// reports the parse error.
func NormalizeBaseURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	u, err := url.Parse(trimmed)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.TrimRight(trimmed, "/")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	u.Host = host
	if strings.HasSuffix(u.Path, "/") {
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
	}
	return u.String()
}

// ValidateBaseURL parses raw as a URL and requires an http(s) scheme and a
// host. It returns the normalized base URL.
func ValidateBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("provider base URL is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid provider base URL %q: %w", raw, err)
	}
	if strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https" {
		return "", fmt.Errorf("invalid provider base URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid provider base URL %q: missing host", raw)
	}
	return NormalizeBaseURL(trimmed), nil
}

// SetQuotaHint records remaining quota as a fraction in [0, 1], or
// QuotaUnknown (-1) when it is not known. Out-of-range values are rejected.
func (p *Provider) SetQuotaHint(hint float64) error {
	if hint != QuotaUnknown && (hint < 0 || hint > 1) {
		return fmt.Errorf("quota hint %v out of range: must be 0..1 or -1 (unknown)", hint)
	}
	p.QuotaHint = hint
	return nil
}

// DeriveName produces a display name from a base URL host, e.g.
// "https://api.codvian.com/v1" -> "Codvian". It strips a leading "api."
// and title-cases the first label. Callers that know a better name should
// pass it explicitly (Registry.AddNamed).
func DeriveName(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return "Provider"
	}
	host := u.Hostname()
	host = strings.TrimPrefix(host, "api.")
	label := host
	if i := strings.IndexByte(host, '.'); i >= 0 {
		label = host[:i]
	}
	if label == "" {
		return "Provider"
	}
	return strings.ToUpper(label[:1]) + label[1:]
}
