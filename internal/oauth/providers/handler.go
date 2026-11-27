// Package providers holds the per-provider OAuth handler registry for
// the LLM OAuth surfaces catalogued as CategoryOAuth in
// internal/catalog (31 providers).
//
// Honesty policy (A3):
//
//   - A handler is implemented ONLY for a flow the provider itself
//     documents (its own docs, its open-source CLI, or the OAuth
//     grant it is built on). Every handler carries a DocsURL citing
//     the source the endpoints came from. Nothing here invents an
//     authorize/token endpoint: a provider whose OAuth surface is not
//     publicly documented stays in the registry as an unimplemented
//     entry with the reason, and its resolution is refused rather
//     than fabricated.
//   - Two providers (antigravity, agy) support paste-token import
//     only: the operator pastes a credential blob exported by the
//     provider's own tooling. Import is allowlisted to exactly those
//     two IDs, and the blob's embedded provider must match the route
//     provider, so one provider's credential can never be stored
//     under another's route.
//
// Storage is NOT this package's job: tokens produced here are stored
// through the server's Vault-sealed OAuth connection store
// (internal/server/oauth_conn.go), and expiry/refresh flow through
// that store's GetAccessToken semantics (refresh 60s before expiry,
// refreshed tokens resealed, never served stale). The bridge lives in
// internal/server/oauth_provider_bridge.go.
package providers

import (
	"context"
	"errors"
	"net/http"
)

// Flow is the OAuth interaction style a provider documents.
type Flow string

const (
	// FlowPKCE is an OAuth 2.0 authorization-code flow with PKCE
	// (RFC 7636) and no client secret (public client).
	FlowPKCE Flow = "pkce"
	// FlowDeviceCode is the OAuth 2.0 device authorization grant
	// (RFC 8628) or a provider-shaped variant of it.
	FlowDeviceCode Flow = "device_code"
	// FlowBrowserSignIn is an authorization-code flow whose client
	// credentials are operator-supplied (the provider documents the
	// endpoints but publishes no universal client ID).
	FlowBrowserSignIn Flow = "browser_sign_in"
	// FlowPasteImport is no interactive flow at all: the operator
	// pastes a credential blob exported by the provider's own
	// tooling (allowlisted import only).
	FlowPasteImport Flow = "paste_import"
)

// Info describes one catalog OAuth provider's handler surface. The
// full list — implemented and not — is the registry's public truth
// (All()).
type Info struct {
	// ID is the catalog provider ID (internal/catalog).
	ID string
	// Name is the human-readable provider name.
	Name string
	// Alias is the catalog short alias, when one exists.
	Alias string
	// Flow is the documented interaction style.
	Flow Flow
	// BaseURL is the provider's LLM API base, when the catalog
	// carries one.
	BaseURL string
	// DocsURL cites the source the flow's endpoints came from.
	// Non-empty for every implemented handler.
	DocsURL string
	// Implemented is true when a handler for this provider exists
	// (a connectable flow or an allowlisted import).
	Implemented bool
	// ImportOnly marks paste-token-import providers: connectable
	// through ImportPasteToken, with no interactive flow.
	ImportOnly bool
	// Reason explains why an unimplemented provider stays
	// unimplemented. Always non-empty when Implemented is false.
	Reason string
}

// TokenResult is the normalized outcome of an exchange or refresh.
// TokenURL is the endpoint a later refresh must hit ("" when the
// provider cannot refresh the stored credential at all).
type TokenResult struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	// ExpiresIn is the token lifetime in seconds; 0 when the
	// provider does not declare one (the store then applies its
	// conservative default).
	ExpiresIn int
	// TokenURL records the refresh endpoint for the connection
	// store.
	TokenURL string
}

// DeviceCode is a device-authorization offer (RFC 8628). The state
// field carries handler-internal continuation data (registered AWS
// client credentials, the device code itself); callers pass the whole
// struct back to PollDevice and never inspect state.
type DeviceCode struct {
	DeviceCode              string
	UserCode                string
	VerificationURI         string
	VerificationURIComplete string
	ExpiresIn               int
	Interval                int

	state map[string]string
}

var (
	// ErrNotCodeFlow is returned by StartAuthorization/ExchangeCode
	// on handlers whose flow is not an authorization-code flow.
	ErrNotCodeFlow = errors.New("providers: flow does not use an authorization URL")
	// ErrNotDeviceFlow is returned by StartDevice/PollDevice on
	// handlers whose flow is not a device flow.
	ErrNotDeviceFlow = errors.New("providers: flow does not use device authorization")
	// ErrAuthorizationPending is returned by PollDevice while the
	// user has not yet approved the device code.
	ErrAuthorizationPending = errors.New("providers: device authorization pending")
	// ErrNoRefresh is returned by Refresh when the provider has no
	// refresh grant for the stored credential.
	ErrNoRefresh = errors.New("providers: flow has no refresh grant")
	// ErrNotImplemented is returned by Get for providers whose
	// OAuth surface is not publicly documented (Info.Reason says
	// why) — the refusal is honest, never a fabricated flow.
	ErrNotImplemented = errors.New("providers: handler not implemented")
	// ErrImportOnly is returned by Get for the paste-token-import
	// providers (antigravity, agy): connect via the allowlisted
	// import, not an interactive flow.
	ErrImportOnly = errors.New("providers: import-only provider (paste-token import)")
)

// Handler is one provider's documented OAuth flow. Implementations
// are stateless: all per-flow state lives in the returned structs, so
// one handler value serves any number of concurrent flows.
type Handler interface {
	// Info reports the provider's registry entry.
	Info() Info
	// StartAuthorization builds the provider's authorization URL for
	// code flows (FlowPKCE, FlowBrowserSignIn). It returns the
	// random state, the URL, and the PKCE verifier ("" when the
	// flow does not use PKCE). The caller binds state to the
	// session and exchanges the returned code with the same
	// redirect URI and verifier.
	StartAuthorization(redirectURI string) (state, authURL, verifier string, err error)
	// ExchangeCode swaps an authorization code for tokens (code
	// flows only; ErrNotCodeFlow otherwise).
	ExchangeCode(ctx context.Context, cl *http.Client, code, redirectURI, verifier string) (*TokenResult, error)
	// StartDevice begins a device flow (FlowDeviceCode only;
	// ErrNotDeviceFlow otherwise).
	StartDevice(ctx context.Context, cl *http.Client) (*DeviceCode, error)
	// PollDevice polls the token endpoint ONCE for a device flow.
	// ErrAuthorizationPending means "not approved yet — poll again
	// after Interval"; any other error is terminal.
	PollDevice(ctx context.Context, cl *http.Client, dc *DeviceCode) (*TokenResult, error)
	// Refresh swaps a refresh token for fresh tokens. ErrNoRefresh
	// when the provider cannot refresh the stored credential kind.
	Refresh(ctx context.Context, cl *http.Client, refreshToken string) (*TokenResult, error)
}

// baseHandler supplies the "not this flow" defaults so each concrete
// handler only overrides what its flow actually uses.
type baseHandler struct{}

func (baseHandler) StartAuthorization(string) (string, string, string, error) {
	return "", "", "", ErrNotCodeFlow
}

func (baseHandler) ExchangeCode(context.Context, *http.Client, string, string, string) (*TokenResult, error) {
	return nil, ErrNotCodeFlow
}

func (baseHandler) StartDevice(context.Context, *http.Client) (*DeviceCode, error) {
	return nil, ErrNotDeviceFlow
}

func (baseHandler) PollDevice(context.Context, *http.Client, *DeviceCode) (*TokenResult, error) {
	return nil, ErrNotDeviceFlow
}

func (baseHandler) Refresh(context.Context, *http.Client, string) (*TokenResult, error) {
	return nil, ErrNoRefresh
}
