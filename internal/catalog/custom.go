package catalog

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// validateBaseURL parses raw as a URL and requires an http(s) scheme and
// a host. It mirrors provider.ValidateBaseURL's contract so this
// stdlib-only package stays independent of internal/provider; the two
// must accept the same URLs. Templates ({resource}, {region},
// {account_id}, {LOCATION}) are NOT accepted here — a custom provider
// needs a concrete base URL. Templated bases exist only in catalog
// entries whose docs define them (Azure, Vertex, Cloudflare, Bedrock).
func validateBaseURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fmt.Errorf("provider base URL is empty")
	}
	if strings.Contains(trimmed, "{") || strings.Contains(trimmed, "}") {
		return fmt.Errorf("invalid provider base URL %q: templates are not allowed here; supply a concrete URL", raw)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid provider base URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid provider base URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("invalid provider base URL %q: missing host", raw)
	}
	return nil
}

// templatePlaceholder matches one {placeholder} in a templated catalog
// base URL (Azure {resource}, Vertex {LOCATION}, Cloudflare
// {account_id}, Bedrock {region}).
var templatePlaceholder = regexp.MustCompile(`\{[A-Za-z0-9_-]+\}`)

// validateCatalogBaseURL validates a catalog entry's base URL. Unlike
// validateBaseURL (custom providers: concrete URLs only), it allows
// {placeholder} templates: they are substituted with a dummy label
// before the same scheme/host checks run. A template is only valid in
// a catalog entry whose docs define it — never in user input.
func validateCatalogBaseURL(raw string) error {
	substituted := templatePlaceholder.ReplaceAllString(strings.TrimSpace(raw), "placeholder")
	return validateBaseURL(substituted)
}

// normalizeBaseURL trims whitespace and trailing slashes. It does not
// add or remove a "/v1" suffix: surfaces differ per provider (DeepSeek's
// chat path has no /v1; Fireworks' Anthropic base has none).
func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// CustomProvider is a user-defined provider: any base URL + key, with
// the user's choice of auth header and wire format. This is the actual
// coverage lead over a fixed catalog: the router never has to wait for
// a catalog update to support a new provider, aggregator, or local
// runtime — if it speaks HTTP, it can be added.
//
// A custom provider is stored as a catalog Provider with Category
// "custom" and Verification "unverified" (the router has not checked
// the user's URL against any docs); ToCustomProvider's Validate still
// enforces a parseable base URL and a coherent auth choice.
type CustomProvider struct {
	// Name is the display name shown in the dashboard and logs.
	Name string
	// BaseURL is the provider's API base, e.g.
	// "https://api.example.com/v1" or "http://localhost:1234/v1".
	BaseURL string
	// AuthKind is how the key is sent: bearer, x-api-key, api-key,
	// x-goog-api-key, x-fireworks-api-key, none, or custom.
	AuthKind AuthKind
	// CustomHeaderName is the header name when AuthKind is AuthCustom
	// (e.g. "X-API-Key"). Ignored for other kinds.
	CustomHeaderName string
	// ExtraHeaders are additional static headers sent on every
	// request (e.g. {"anthropic-version": "2026-10-21"}).
	ExtraHeaders map[string]string
	// WireFormat is the request shape the endpoint speaks.
	WireFormat WireFormat
}

// Custom-provider prefix convention (PARITY Area 3, "Custom-provider
// prefix convention"). A custom provider ID may carry one of three
// prefixes naming the wire format it speaks, so an operator (or a
// config file, or the add-provider API) can say what an endpoint is
// without spelling out the wire format separately:
//
//	openai-compatible-<name>    OpenAI Chat Completions (/v1/chat/completions)
//	anthropic-compatible-<name> Anthropic Messages (/v1/messages)
//	anthropic-compatible-cc-<name> Claude-Code-compatible Anthropic surface
//
// The "cc" variant is the Claude Code surface: an Anthropic Messages
// endpoint reached through a Claude Code-compatible router. It maps
// to the same WireAnthropic format — the prefix records provenance,
// the wire is what the router executes. An ID with no known prefix
// is a generic custom provider: the wire format stays whatever the
// operator chose (WireOpenAI by default in ToProvider).
type CustomPrefix string

const (
	// CustomPrefixOpenAICompatible marks an OpenAI-compatible endpoint.
	CustomPrefixOpenAICompatible CustomPrefix = "openai-compatible-"
	// CustomPrefixAnthropicCompatible marks an Anthropic-compatible endpoint.
	CustomPrefixAnthropicCompatible CustomPrefix = "anthropic-compatible-"
	// CustomPrefixAnthropicCompatibleCC marks a Claude-Code-compatible
	// Anthropic endpoint.
	CustomPrefixAnthropicCompatibleCC CustomPrefix = "anthropic-compatible-cc-"
)

// customPrefixes is the match order: longest first, so
// "anthropic-compatible-cc-..." matches the cc variant before the
// plain Anthropic one.
var customPrefixes = []struct {
	prefix CustomPrefix
	wire   WireFormat
}{
	{CustomPrefixAnthropicCompatibleCC, WireAnthropic},
	{CustomPrefixAnthropicCompatible, WireAnthropic},
	{CustomPrefixOpenAICompatible, WireOpenAI},
}

// ParseCustomPrefix splits a custom-provider ID into its prefix
// convention, the remainder, and the wire format it implies.
// ok is false when the ID carries no known prefix. The remainder
// must be non-empty: a bare prefix names no provider. Prefixes are
// tried longest-first, and the first prefix the ID starts with is
// decisive: "anthropic-compatible-cc-" is a bare cc prefix, not
// "anthropic-compatible-" + "cc-".
func ParseCustomPrefix(id string) (prefix CustomPrefix, rest string, wire WireFormat, ok bool) {
	for _, cp := range customPrefixes {
		if r, found := strings.CutPrefix(id, string(cp.prefix)); found {
			if strings.TrimSpace(r) == "" {
				return "", "", WireUnknown, false
			}
			return cp.prefix, r, cp.wire, true
		}
	}
	return "", "", WireUnknown, false
}

// ApplyPrefix derives the wire format from a prefixed ID and sets
// it on the custom provider, returning true when the ID carried a
// known prefix. The prefix wins over a previously set WireFormat:
// it is the operator's specific claim about what the endpoint
// speaks, and a WireFormat contradicting it is a configuration
// mistake. Unprefixed IDs leave the provider untouched.
func (c *CustomProvider) ApplyPrefix(id string) bool {
	_, _, wire, ok := ParseCustomPrefix(id)
	if !ok {
		return false
	}
	c.WireFormat = wire
	return true
}

// Validate checks the custom provider's configuration. A key is not
// validated here (keys are validated by the upstream on first use, and
// key formats are unreliable validators).
func (c CustomProvider) Validate() error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("catalog: custom provider name is empty")
	}
	if err := validateBaseURL(c.BaseURL); err != nil {
		return err
	}
	switch c.AuthKind {
	case AuthBearer, AuthXAPIKey, AuthAPIKey, AuthXGoogAPIKey, AuthXFireworksAPIKey, AuthNone:
		// Known header kinds need no further input.
	case AuthCustom:
		if strings.TrimSpace(c.CustomHeaderName) == "" {
			return fmt.Errorf("catalog: custom provider %q uses custom auth but no header name was given", c.Name)
		}
	default:
		return fmt.Errorf("catalog: custom provider %q: auth kind %q is not usable for a custom provider (exchange/oauth/cookie need a catalog entry)", c.Name, c.AuthKind)
	}
	switch c.WireFormat {
	case WireOpenAI, WireAnthropic, WireGemini, WireCohere, WireOllama, WireUnknown:
	default:
		return fmt.Errorf("catalog: custom provider %q: unknown wire format %q", c.Name, c.WireFormat)
	}
	return nil
}

// headerName returns the header carrying the credential for this custom
// provider's auth kind.
func (c CustomProvider) headerName() string {
	switch c.AuthKind {
	case AuthBearer:
		return "Authorization"
	case AuthXAPIKey:
		return "x-api-key"
	case AuthAPIKey:
		return "api-key"
	case AuthXGoogAPIKey:
		return "x-goog-api-key"
	case AuthXFireworksAPIKey:
		return "X-Fireworks-Api-Key"
	case AuthCustom:
		return strings.TrimSpace(c.CustomHeaderName)
	default: // AuthNone
		return ""
	}
}

// ToProvider converts the custom provider into a catalog Provider so
// the rest of the router (registry, auth resolver, dashboard) treats
// it exactly like a built-in entry. The ID is derived from the name
// and the current time so two custom providers with the same name do
// not collide.
//
// A name carrying a known prefix (openai-compatible-,
// anthropic-compatible-, anthropic-compatible-cc-) states the wire
// format: the prefix wins over an explicitly set WireFormat, and the
// slug is derived from the remainder, so the generated ID does not
// repeat the prefix ("anthropic-compatible-foo" → "custom-foo-<ts>").
// The display Name keeps the full prefixed name.
func (c CustomProvider) ToProvider() (Provider, error) {
	if err := c.Validate(); err != nil {
		return Provider{}, err
	}
	// The prefix convention (PARITY B4-04): apply the prefix from
	// the name before converting, so a prefixed custom provider
	// converts with the wire format the prefix promises. Before
	// this, ParseCustomPrefix/ApplyPrefix existed but ToProvider
	// never called them, so a prefixed name converted with the
	// default WireOpenAI (wrong for anthropic-compatible-*) and a
	// slug that repeated the prefix. c is a value copy, so the
	// caller's struct is untouched.
	c.ApplyPrefix(c.Name)
	wire := c.WireFormat
	if wire == "" {
		wire = WireOpenAI // the common case; the user can change it
	}
	extra := make(map[string]string, len(c.ExtraHeaders))
	for k, v := range c.ExtraHeaders {
		extra[k] = v
	}
	slugSource := strings.TrimSpace(c.Name)
	if _, rest, _, ok := ParseCustomPrefix(slugSource); ok {
		slugSource = rest
	}
	slug := strings.ToLower(slugSource)
	slug = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r == ' ' || r == '_' || r == '.':
			return '-'
		default:
			return -1
		}
	}, slug)
	if slug == "" {
		slug = "custom"
	}
	return Provider{
		ID:           "custom-" + slug + "-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Name:         c.Name,
		Category:     CategoryCustom,
		BaseURLs:     []string{normalizeBaseURL(c.BaseURL)},
		WireFormats:  []WireFormat{wire},
		Endpoints:    []EndpointAuth{{Endpoint: EndpointChat, BaseURL: normalizeBaseURL(c.BaseURL), AuthKind: c.AuthKind, HeaderName: c.headerName(), ExtraHeaders: extra}},
		Verification: VerificationUnverified,
		SourceURL:    "",
		Notes:        "User-defined custom provider; base URL and auth supplied by the user, not verified against docs.",
	}, nil
}
