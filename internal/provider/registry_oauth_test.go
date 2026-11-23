package provider

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// chatgptOAuthProvider is a catalog-linked provider in the OAuth
// category (ToSGrey=false, no endpoint table, so DefaultAuthKind is
// AuthOAuth). It is a realistic non-grey OAuth routing target.
func chatgptOAuthProvider() *Provider {
	return &Provider{
		ID:        "p-chatgpt",
		Name:      "ChatGPT",
		CatalogID: "chatgpt",
		BaseURL:   "https://chatgpt.com/backend-api",
		Enabled:   true,
	}
}

// TestResolveAuthHeadersOAuthBearer verifies that an OAuth surface
// resolves to Authorization: Bearer <token> using the wired resolver,
// and carries the endpoint's extra headers.
func TestResolveAuthHeadersOAuthBearer(t *testing.T) {
	reg := NewRegistry(nil)
	p := chatgptOAuthProvider()
	reg.SetOAuthTokenResolver(func(id string) (string, error) {
		if id != "chatgpt" {
			t.Errorf("resolver called with id %q, want chatgpt", id)
		}
		return "oauth-access-token-123", nil
	})
	headers, wire, expiry, err := reg.resolveAuthHeadersUncached(p, "ignored-static-key")
	if err != nil {
		t.Fatalf("resolveAuthHeadersUncached: %v", err)
	}
	if got := headers["Authorization"]; got != "Bearer oauth-access-token-123" {
		t.Errorf("Authorization = %q, want Bearer oauth-access-token-123", got)
	}
	if wire == "" {
		t.Errorf("wire is empty, want a non-empty wire format")
	}
	// OAuth resolutions carry a short TTL so refreshed tokens replace
	// the cached header.
	if expiry.IsZero() || time.Now().After(expiry) {
		t.Errorf("expiry = %v, want a near-future TTL", expiry)
	}
}

// TestResolveAuthHeadersOAuthNoResolver verifies an OAuth surface
// stays refused (never fabricated) when no resolver is wired.
func TestResolveAuthHeadersOAuthNoResolver(t *testing.T) {
	reg := NewRegistry(nil)
	p := chatgptOAuthProvider()
	_, _, _, err := reg.resolveAuthHeadersUncached(p, "ignored-static-key")
	if err == nil {
		t.Fatal("expected error for OAuth surface with no resolver, got nil")
	}
	if !strings.Contains(err.Error(), "no OAuth token resolver") {
		t.Errorf("error = %v, want it to mention missing resolver", err)
	}
}

// TestResolveAuthHeadersOAuthResolverError verifies a resolver
// failure surfaces as an honest error, not a fake header.
func TestResolveAuthHeadersOAuthResolverError(t *testing.T) {
	reg := NewRegistry(nil)
	p := chatgptOAuthProvider()
	reg.SetOAuthTokenResolver(func(id string) (string, error) {
		return "", errors.New("oauth: no connection for \"chatgpt\"")
	})
	_, _, _, err := reg.resolveAuthHeadersUncached(p, "")
	if err == nil {
		t.Fatal("expected error when resolver fails, got nil")
	}
	if !strings.Contains(err.Error(), "no connection") {
		t.Errorf("error = %v, want it to wrap the resolver error", err)
	}
}

// TestResolveAuthHeadersOAuthEmptyToken verifies an empty token is
// rejected rather than sent as an empty Bearer.
func TestResolveAuthHeadersOAuthEmptyToken(t *testing.T) {
	reg := NewRegistry(nil)
	p := chatgptOAuthProvider()
	reg.SetOAuthTokenResolver(func(id string) (string, error) { return "   ", nil })
	_, _, _, err := reg.resolveAuthHeadersUncached(p, "")
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "empty token") {
		t.Errorf("error = %v, want it to mention empty token", err)
	}
}

// TestIsOAuthExchangeKind covers the exchange-kind classifier.
func TestIsOAuthExchangeKind(t *testing.T) {
	cases := map[string]bool{
		"oauth-pkce":           true,
		"oauth-device-code":    true,
		"OAuth-PKCE":           true,
		"sigv4":                false,
		"service-account-json": false,
		"entra-id":             false,
		"":                     false,
	}
	for kind, want := range cases {
		if got := isOAuthExchangeKind(kind); got != want {
			t.Errorf("isOAuthExchangeKind(%q) = %v, want %v", kind, got, want)
		}
	}
}
