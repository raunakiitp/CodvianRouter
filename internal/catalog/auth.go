package catalog

import (
	"errors"
	"fmt"
	"strings"
)

// ErrExchangeRequired is returned by ResolveAuth when the requested
// surface needs a credential exchange (service-account JSON, SigV4,
// JWT minting, OAuth login) instead of a static header. The caller must
// perform the exchange described in AuthResolution.Exchange; the
// resolver never fabricates an Authorization value for these surfaces.
var ErrExchangeRequired = errors.New("catalog: credential exchange required; no static header exists for this surface")

// ErrUnknownEndpoint is returned when the provider records endpoints
// but the requested surface is not among them.
var ErrUnknownEndpoint = errors.New("catalog: unknown endpoint for provider")

// ErrCredentialRequired is returned when a surface needs a credential
// and none was supplied.
var ErrCredentialRequired = errors.New("catalog: credential required for this surface")

// AuthResolution is the result of resolving auth for one
// (provider, endpoint) pair.
type AuthResolution struct {
	// Headers are the exact headers to send on the upstream request.
	// Empty for AuthNone surfaces, nil when Exchange is set.
	Headers map[string]string
	// BaseURL is the base to send the request to (endpoint-specific
	// when recorded, else the provider's first BaseURLs entry, else "").
	BaseURL string
	// Exchange is non-nil when the credential must be exchanged first.
	// Headers is nil in that case.
	Exchange *ExchangeAuth
	// Wire is the wire format this surface speaks.
	Wire WireFormat
}

// ResolveAuth picks the correct auth for one (provider, endpoint) pair
// and builds the request headers from the caller's stored credential.
//
// Rules:
//   - The header is chosen per (provider, endpoint), not per provider:
//     DeepSeek needs Bearer on its OpenAI surface and x-api-key on its
//     Anthropic surface; Fireworks takes Bearer or X-Fireworks-Api-Key;
//     Azure takes api-key on legacy REST and Bearer (Entra) on Foundry.
//   - endpoint "" means the provider's default surface.
//   - variant "" means the "default" variant of that surface.
//   - Exchange surfaces (AuthExchange, AuthOAuth) return
//     ErrExchangeRequired with Exchange set and no headers — the
//     credential exchange must happen first, in the caller, and is
//     never faked as a static header.
//   - AuthNone surfaces return empty headers and no error, even with
//     an empty credential.
//   - A provider with no recorded Endpoints falls back to its default
//     auth (Bearer on BaseURLs[0] for API-key providers).
//   - Unknown endpoint on a provider that DOES record endpoints is an
//     error, not a silent Bearer fallback: sending the wrong header to
//     a known surface is worse than failing loudly.
//   - Key prefixes are never validated here: providers publish no
//     prefix for many keys, and a prefix mismatch must never reject a
//     working key.
func ResolveAuth(p Provider, endpoint, variant, credential string) (AuthResolution, error) {
	if endpoint == "" {
		// Default surface: chat when the provider has one, else its
		// first recorded surface (Anthropic's is "messages", Gemini's
		// is "generate" — neither has a "chat" endpoint).
		if _, ok := p.Endpoint(EndpointChat, ""); ok {
			endpoint = EndpointChat
		} else if len(p.Endpoints) > 0 {
			endpoint = p.Endpoints[0].Endpoint
			variant = p.Endpoints[0].Variant
		} else {
			endpoint = EndpointChat
		}
	}
	ep, ok := p.Endpoint(endpoint, variant)
	if !ok {
		if len(p.Endpoints) > 0 {
			return AuthResolution{}, fmt.Errorf("%w: provider %q has no %q endpoint", ErrUnknownEndpoint, p.ID, endpoint)
		}
		ep = p.DefaultEndpointAuth()
	}

	base := ep.BaseURL
	if base == "" && len(p.BaseURLs) > 0 {
		base = p.BaseURLs[0]
	}
	wire := WireUnknown
	if len(p.WireFormats) > 0 {
		wire = p.WireFormats[0]
	}
	// A surface whose base URL names the Anthropic path speaks the
	// Anthropic wire; a Gemini base speaks Gemini; an OpenAI
	// compatibility shim speaks OpenAI wire even when the provider's
	// preferred (first) wire is native (Cohere's
	// /compatibility/v1 shim is OpenAI wire, its native base is
	// Cohere v2). This keeps the reported wire honest for
	// multi-surface providers whose WireFormats list is ordered by
	// preference.
	switch {
	case strings.Contains(base, "/anthropic"):
		wire = WireAnthropic
	case strings.Contains(base, "generativelanguage.googleapis.com") ||
		strings.Contains(base, "aiplatform.googleapis.com"):
		wire = WireGemini
	case strings.Contains(base, "/compatibility/v1"):
		wire = WireOpenAI
	case strings.Contains(base, "bedrock-runtime") || strings.Contains(base, "bedrock-mantle"):
		if ep.Endpoint == EndpointConverse {
			wire = WireBedrock
		}
	}

	switch ep.AuthKind {
	case AuthNone:
		return AuthResolution{Headers: map[string]string{}, BaseURL: base, Wire: wire}, nil
	case AuthExchange, AuthOAuth:
		if ep.Exchange == nil {
			return AuthResolution{}, fmt.Errorf("catalog: provider %q endpoint %q: %s auth without an exchange description", p.ID, endpoint, ep.AuthKind)
		}
		ex := *ep.Exchange
		return AuthResolution{BaseURL: base, Wire: wire, Exchange: &ex}, fmt.Errorf("%w: %s (%s)", ErrExchangeRequired, ex.Kind, ex.Description)
	case AuthCookie:
		return AuthResolution{}, fmt.Errorf("catalog: provider %q endpoint %q uses web-cookie auth, which is ToS-grey and not supported as a routing target", p.ID, endpoint)
	}

	if strings.TrimSpace(credential) == "" {
		return AuthResolution{}, fmt.Errorf("%w: provider %q endpoint %q", ErrCredentialRequired, p.ID, endpoint)
	}
	if strings.TrimSpace(ep.HeaderName) == "" {
		return AuthResolution{}, fmt.Errorf("catalog: provider %q endpoint %q: %s auth without a header name", p.ID, endpoint, ep.AuthKind)
	}
	headers := make(map[string]string, len(ep.ExtraHeaders)+1)
	for k, v := range ep.ExtraHeaders {
		headers[k] = v
	}
	if ep.AuthKind == AuthBearer {
		headers[ep.HeaderName] = "Bearer " + credential
	} else {
		headers[ep.HeaderName] = credential
	}
	return AuthResolution{Headers: headers, BaseURL: base, Wire: wire}, nil
}

// ResolveAuthForBaseURL resolves auth for a raw base URL against the
// catalog: it finds the provider whose BaseURLs or endpoint BaseURLs
// contain the URL (exact match after trimming trailing slashes) and
// resolves that provider's matching endpoint. It is how a custom or
// imported provider config finds its auth without an ID. It returns
// false when no catalog entry claims the URL.
//
// Endpoint base URLs are matched first: a base URL that IS one
// surface's base (e.g. https://api.deepseek.com/anthropic) is that
// surface, so its auth (x-api-key) wins over the provider's generic
// base (which would resolve the chat surface's Bearer instead).
func ResolveAuthForBaseURL(baseURL, endpoint, variant, credential string) (AuthResolution, bool, error) {
	normalized := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	for _, p := range All() {
		for _, e := range p.Endpoints {
			if e.BaseURL != "" && strings.TrimRight(strings.TrimSpace(e.BaseURL), "/") == normalized {
				res, err := ResolveAuth(p, e.Endpoint, e.Variant, credential)
				return res, true, err
			}
		}
	}
	for _, p := range All() {
		for _, u := range p.BaseURLs {
			if strings.TrimRight(strings.TrimSpace(u), "/") == normalized {
				res, err := ResolveAuth(p, endpoint, variant, credential)
				return res, true, err
			}
		}
	}
	return AuthResolution{}, false, nil
}
