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

// WireFormatOpenAI is the default (and currently only supported) wire
// format: OpenAI-compatible /v1/models and /v1/chat/completions.
const WireFormatOpenAI = "openai"

// WireFormatAnthropic is the Anthropic Messages wire format
// (POST /v1/messages), spoken by internal/wire. A provider whose
// catalog wire format resolves to this speaks Messages, not OpenAI
// chat completions; see NewClient (client.go).
const WireFormatAnthropic = "anthropic"

// WireFormatUnknown means auto-detection could not confirm an
// OpenAI-compatible endpoint. The provider is still stored, but routing
// to it should be treated as best-effort.
const WireFormatUnknown = "unknown"

// QuotaUnknown is the QuotaHint value meaning "remaining quota not known".
const QuotaUnknown = -1.0

// Model is a single model offered by a provider. ContextWindow and prices
// are best-effort metadata: 0 means unknown (most upstreams do not report
// pricing via /v1/models, and it is filled in later if known).
type Model struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	ContextWindow  int     `json:"context_window,omitempty"`
	InputPriceUSD  float64 `json:"input_price_usd,omitempty"`
	OutputPriceUSD float64 `json:"output_price_usd,omitempty"`
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
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	BaseURL         string    `json:"base_url"`
	APIKeyEncrypted string    `json:"api_key_encrypted"`
	WireFormat      string    `json:"wire_format"`
	Models          []Model   `json:"models,omitempty"`
	Enabled         bool      `json:"enabled"`
	QuotaHint       float64   `json:"quota_hint"`
	CreatedAt       time.Time `json:"created_at"`
	CatalogID       string    `json:"catalog_id,omitempty"`
	Verification    string    `json:"verification,omitempty"`
	AuthEndpoint    string    `json:"auth_endpoint,omitempty"`
	AuthVariant     string    `json:"auth_variant,omitempty"`
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
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	BaseURL         string    `json:"base_url"`
	APIKeyEncrypted string    `json:"api_key_encrypted"`
	WireFormat      string    `json:"wire_format"`
	Models          []Model   `json:"models,omitempty"`
	Enabled         bool      `json:"enabled"`
	QuotaHint       float64   `json:"quota_hint"`
	CreatedAt       time.Time `json:"created_at"`
	CatalogID       string    `json:"catalog_id,omitempty"`
	Verification    string    `json:"verification,omitempty"`
	AuthEndpoint    string    `json:"auth_endpoint,omitempty"`
	AuthVariant     string    `json:"auth_variant,omitempty"`
}

// ToConfig converts a Provider to its persisted form.
func (p *Provider) ToConfig() ProviderConfig {
	if p == nil {
		return ProviderConfig{}
	}
	models := make([]Model, len(p.Models))
	copy(models, p.Models)
	return ProviderConfig{
		ID:              p.ID,
		Name:            p.Name,
		BaseURL:         p.BaseURL,
		APIKeyEncrypted: p.APIKeyEncrypted,
		WireFormat:      p.WireFormat,
		Models:          models,
		Enabled:         p.Enabled,
		QuotaHint:       p.QuotaHint,
		CreatedAt:       p.CreatedAt,
		CatalogID:       p.CatalogID,
		Verification:    p.Verification,
		AuthEndpoint:    p.AuthEndpoint,
		AuthVariant:     p.AuthVariant,
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
	return &Provider{
		ID:              c.ID,
		Name:            c.Name,
		BaseURL:         NormalizeBaseURL(c.BaseURL),
		APIKeyEncrypted: c.APIKeyEncrypted,
		WireFormat:      c.WireFormat,
		Models:          models,
		Enabled:         c.Enabled,
		QuotaHint:       c.QuotaHint,
		CreatedAt:       c.CreatedAt,
		CatalogID:       c.CatalogID,
		Verification:    c.Verification,
		AuthEndpoint:    c.AuthEndpoint,
		AuthVariant:     c.AuthVariant,
	}
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
	return &c
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

// NormalizeBaseURL trims whitespace and trailing slashes. It does not add
// or remove a "/v1" suffix: the client appends "/v1/..." when the base
// path does not already end in "/v1" (see joinAPIPath in client.go).
func NormalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
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
	if u.Scheme != "http" && u.Scheme != "https" {
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
