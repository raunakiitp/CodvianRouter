package catalog

import (
	"errors"
	"testing"
)

func mustProvider(t *testing.T, id string) Provider {
	t.Helper()
	p, ok := ByID(id)
	if !ok {
		t.Fatalf("provider %q not in catalog", id)
	}
	return p
}

func TestResolveAuthBearer(t *testing.T) {
	p := mustProvider(t, "openai")
	res, err := ResolveAuth(p, EndpointChat, "", "sk-test-key")
	if err != nil {
		t.Fatalf("ResolveAuth openai chat: %v", err)
	}
	if res.Headers["Authorization"] != "Bearer sk-test-key" {
		t.Fatalf("Authorization = %q", res.Headers["Authorization"])
	}
	if res.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("base URL = %q", res.BaseURL)
	}
	if res.Wire != WireOpenAI {
		t.Fatalf("wire = %q", res.Wire)
	}
}

func TestResolveAuthAnthropic(t *testing.T) {
	p := mustProvider(t, "anthropic")
	// Default surface is messages; Bearer is preferred.
	res, err := ResolveAuth(p, "", "", "sk-ant-test")
	if err != nil {
		t.Fatalf("ResolveAuth anthropic default: %v", err)
	}
	if res.Headers["Authorization"] != "Bearer sk-ant-test" {
		t.Fatalf("Authorization = %q", res.Headers["Authorization"])
	}
	if res.Headers["anthropic-version"] != AnthropicVersion {
		t.Fatalf("anthropic-version = %q", res.Headers["anthropic-version"])
	}
	if res.Wire != WireAnthropic {
		t.Fatalf("wire = %q", res.Wire)
	}
	// Legacy x-api-key variant.
	res, err = ResolveAuth(p, EndpointMessages, "x-api-key", "sk-ant-test")
	if err != nil {
		t.Fatalf("ResolveAuth anthropic x-api-key: %v", err)
	}
	if _, ok := res.Headers["Authorization"]; ok {
		t.Fatal("x-api-key variant must not set Authorization")
	}
	if res.Headers["x-api-key"] != "sk-ant-test" {
		t.Fatalf("x-api-key = %q", res.Headers["x-api-key"])
	}
	if res.Headers["anthropic-version"] != AnthropicVersion {
		t.Fatalf("anthropic-version = %q", res.Headers["anthropic-version"])
	}
}

func TestResolveAuthGemini(t *testing.T) {
	p := mustProvider(t, "gemini")
	res, err := ResolveAuth(p, EndpointGenerate, "", "test-key")
	if err != nil {
		t.Fatalf("ResolveAuth gemini: %v", err)
	}
	if res.Headers["x-goog-api-key"] != "test-key" {
		t.Fatalf("x-goog-api-key = %q", res.Headers["x-goog-api-key"])
	}
	if _, ok := res.Headers["Authorization"]; ok {
		t.Fatal("gemini must not use Authorization")
	}
	if res.Wire != WireGemini {
		t.Fatalf("wire = %q", res.Wire)
	}
}

func TestResolveAuthDeepSeekPerSurface(t *testing.T) {
	p := mustProvider(t, "deepseek")
	chat, err := ResolveAuth(p, EndpointChat, "", "k")
	if err != nil {
		t.Fatalf("deepseek chat: %v", err)
	}
	if chat.Headers["Authorization"] != "Bearer k" {
		t.Fatalf("deepseek chat Authorization = %q", chat.Headers["Authorization"])
	}
	if chat.BaseURL != "https://api.deepseek.com" {
		t.Fatalf("deepseek chat base = %q (must have NO /v1)", chat.BaseURL)
	}
	anth, err := ResolveAuth(p, EndpointAnthropic, "", "k")
	if err != nil {
		t.Fatalf("deepseek anthropic: %v", err)
	}
	if anth.Headers["x-api-key"] != "k" {
		t.Fatalf("deepseek anthropic x-api-key = %q", anth.Headers["x-api-key"])
	}
	if anth.BaseURL != "https://api.deepseek.com/anthropic" {
		t.Fatalf("deepseek anthropic base = %q", anth.BaseURL)
	}
}

func TestResolveAuthFireworksAltHeader(t *testing.T) {
	p := mustProvider(t, "fireworks")
	res, err := ResolveAuth(p, EndpointChat, "alt-header", "fw_test")
	if err != nil {
		t.Fatalf("fireworks alt-header: %v", err)
	}
	if res.Headers["X-Fireworks-Api-Key"] != "fw_test" {
		t.Fatalf("X-Fireworks-Api-Key = %q", res.Headers["X-Fireworks-Api-Key"])
	}
	if _, ok := res.Headers["Authorization"]; ok {
		t.Fatal("alt-header variant must not set Authorization")
	}
}

func TestResolveAuthExchangeNeverFakesHeader(t *testing.T) {
	cases := []struct {
		id       string
		endpoint string
		variant  string
		wantKind string
	}{
		{"vertex", EndpointGenerate, "", "service-account-json"},
		{"bedrock", EndpointConverse, "", "sigv4"},
		{"openrouter", EndpointChat, "jwt", "jwt"},
		{"azure-openai", EndpointChat, "", "entra-id"},
		{"claude", EndpointMessages, "", "oauth-pkce"},
	}
	for _, tc := range cases {
		p := mustProvider(t, tc.id)
		res, err := ResolveAuth(p, tc.endpoint, tc.variant, "stored-credential")
		if !errors.Is(err, ErrExchangeRequired) {
			t.Fatalf("%s %s: err = %v, want ErrExchangeRequired", tc.id, tc.endpoint, err)
		}
		if res.Headers != nil {
			t.Fatalf("%s %s: headers must be nil on exchange, got %v", tc.id, tc.endpoint, res.Headers)
		}
		if res.Exchange == nil || res.Exchange.Kind != tc.wantKind {
			t.Fatalf("%s %s: exchange kind = %+v, want %q", tc.id, tc.endpoint, res.Exchange, tc.wantKind)
		}
	}
}

func TestResolveAuthAzureAPIKeyVariant(t *testing.T) {
	p := mustProvider(t, "azure-openai")
	res, err := ResolveAuth(p, EndpointChat, "api-key", "azure-key")
	if err != nil {
		t.Fatalf("azure api-key variant: %v", err)
	}
	if res.Headers["api-key"] != "azure-key" {
		t.Fatalf("api-key = %q", res.Headers["api-key"])
	}
	if _, ok := res.Headers["Authorization"]; ok {
		t.Fatal("api-key variant must not set Authorization")
	}
}

func TestResolveAuthNoAuth(t *testing.T) {
	p := mustProvider(t, "ollama-local")
	res, err := ResolveAuth(p, EndpointChat, "", "")
	if err != nil {
		t.Fatalf("ollama chat with empty credential: %v", err)
	}
	if len(res.Headers) != 0 {
		t.Fatalf("ollama headers = %v, want empty", res.Headers)
	}
}

func TestResolveAuthErrors(t *testing.T) {
	openai := mustProvider(t, "openai")
	if _, err := ResolveAuth(openai, "bogus-endpoint", "", "k"); !errors.Is(err, ErrUnknownEndpoint) {
		t.Fatalf("unknown endpoint err = %v, want ErrUnknownEndpoint", err)
	}
	if _, err := ResolveAuth(openai, EndpointChat, "", ""); !errors.Is(err, ErrCredentialRequired) {
		t.Fatalf("empty credential err = %v, want ErrCredentialRequired", err)
	}
	// A web-cookie provider must refuse to resolve as a routing target.
	cookies := ByCategory(CategoryWebCookie)
	if len(cookies) == 0 {
		t.Skip("no web-cookie providers in catalog")
	}
	if _, err := ResolveAuth(cookies[0], EndpointChat, "", "cookie"); err == nil {
		t.Fatalf("web-cookie provider %q resolved without error", cookies[0].ID)
	}
}

func TestResolveAuthForBaseURL(t *testing.T) {
	res, found, err := ResolveAuthForBaseURL("https://api.openai.com/v1", EndpointChat, "", "k")
	if err != nil || !found {
		t.Fatalf("ResolveAuthForBaseURL openai: found=%v err=%v", found, err)
	}
	if res.Headers["Authorization"] != "Bearer k" {
		t.Fatalf("Authorization = %q", res.Headers["Authorization"])
	}
	_, found, err = ResolveAuthForBaseURL("https://no-such-provider.example.com/v1", EndpointChat, "", "k")
	if found || err != nil {
		t.Fatalf("unknown base URL: found=%v err=%v", found, err)
	}

	// An endpoint's own base URL resolves that surface's auth, not the
	// provider's generic base: DeepSeek's /anthropic base takes
	// x-api-key, even though the provider's chat surface is Bearer.
	res, found, err = ResolveAuthForBaseURL("https://api.deepseek.com/anthropic", EndpointChat, "", "k")
	if err != nil || !found {
		t.Fatalf("ResolveAuthForBaseURL deepseek/anthropic: found=%v err=%v", found, err)
	}
	if res.Headers["x-api-key"] != "k" {
		t.Fatalf("x-api-key = %q, want k", res.Headers["x-api-key"])
	}
	if _, ok := res.Headers["Authorization"]; ok {
		t.Fatalf("Authorization must not be set on the Anthropic surface, got %q", res.Headers["Authorization"])
	}
}

func TestCustomProvider(t *testing.T) {
	// Happy path: any OpenAI-compatible endpoint works.
	c := CustomProvider{Name: "My Gateway", BaseURL: "https://api.example.com/v1", AuthKind: AuthBearer, WireFormat: WireOpenAI}
	p, err := c.ToProvider()
	if err != nil {
		t.Fatalf("ToProvider: %v", err)
	}
	if p.Category != CategoryCustom || p.Verification != VerificationUnverified {
		t.Fatalf("custom provider category=%q verification=%q", p.Category, p.Verification)
	}
	res, err := ResolveAuth(p, EndpointChat, "", "key123")
	if err != nil {
		t.Fatalf("ResolveAuth custom: %v", err)
	}
	if res.Headers["Authorization"] != "Bearer key123" {
		t.Fatalf("Authorization = %q", res.Headers["Authorization"])
	}
	// Custom header name.
	c2 := CustomProvider{Name: "Weird", BaseURL: "https://x.example.com/v1", AuthKind: AuthCustom, CustomHeaderName: "X-My-Key", WireFormat: WireOpenAI}
	p2, err := c2.ToProvider()
	if err != nil {
		t.Fatalf("ToProvider custom header: %v", err)
	}
	res, err = ResolveAuth(p2, EndpointChat, "", "k")
	if err != nil || res.Headers["X-My-Key"] != "k" {
		t.Fatalf("custom header resolve: headers=%v err=%v", res.Headers, err)
	}
	// Failures.
	for _, bad := range []CustomProvider{
		{Name: "", BaseURL: "https://x.example.com/v1", AuthKind: AuthBearer},
		{Name: "Bad", BaseURL: "not a url", AuthKind: AuthBearer},
		{Name: "Bad", BaseURL: "ftp://x.example.com/v1", AuthKind: AuthBearer},
		{Name: "Bad", BaseURL: "https://x.example.com/v1", AuthKind: AuthCustom},
		{Name: "Bad", BaseURL: "https://x.example.com/v1", AuthKind: AuthExchange},
		{Name: "Bad", BaseURL: "https://{resource}.example.com/v1", AuthKind: AuthBearer},
	} {
		if _, err := bad.ToProvider(); err == nil {
			t.Fatalf("ToProvider(%+v) should have failed", bad)
		}
	}
	// Local runtime, no auth.
	local := CustomProvider{Name: "Local vLLM", BaseURL: "http://localhost:8000/v1", AuthKind: AuthNone, WireFormat: WireOpenAI}
	pl, err := local.ToProvider()
	if err != nil {
		t.Fatalf("local ToProvider: %v", err)
	}
	res, err = ResolveAuth(pl, EndpointChat, "", "")
	if err != nil || len(res.Headers) != 0 {
		t.Fatalf("local resolve: headers=%v err=%v", res.Headers, err)
	}
}
