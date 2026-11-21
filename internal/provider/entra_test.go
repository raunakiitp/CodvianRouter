package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// entraTokenMock is a local stand-in for the Microsoft Entra token
// endpoint: it records the form of every mint and answers with a
// fixed access token.
type entraTokenMock struct {
	srv       *httptest.Server
	calls     atomic.Int32
	lastForm  url.Values
	status    int
	expiresIn int
	body      string
}

func newEntraTokenMock(t *testing.T) *entraTokenMock {
	t.Helper()
	m := &entraTokenMock{status: 200, expiresIn: 3600}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		_ = r.ParseForm()
		m.lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.status)
		if m.body != "" {
			_, _ = w.Write([]byte(m.body))
			return
		}
		_, _ = fmt.Fprintf(w, `{"access_token":"entra-token-%d","expires_in":%d,"token_type":"Bearer"}`, m.calls.Load(), m.expiresIn)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

// entraTestCred builds a client-credential JSON pointing at the mock
// token endpoint. clientID is unique per test so the process-wide
// token cache never serves one test's token to another.
func entraTestCred(tokenURI, clientID string) string {
	b, _ := json.Marshal(map[string]string{
		"tenant_id":     "00000000-0000-0000-0000-000000000001",
		"client_id":     clientID,
		"client_secret": "test-secret-value",
		"token_uri":     tokenURI,
	})
	return string(b)
}

func TestParseEntraCredentials(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		c, err := parseEntraCredentials(`{"tenant_id":"contoso.onmicrosoft.com","client_id":"app-1","client_secret": "***"}`)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.TokenURI != "https://login.microsoftonline.com/contoso.onmicrosoft.com/oauth2/v2.0/token" {
			t.Errorf("default token_uri = %q", c.TokenURI)
		}
		if c.Scope != entraDefaultScope {
			t.Errorf("default scope = %q", c.Scope)
		}
	})
	bad := []string{
		`not json`,
		`{"client_id":"a","client_secret": "***"}`,
		`{"tenant_id":"t","client_secret": "***"}`,
		`{"tenant_id":"t","client_id":"a"}`,
		`{"tenant_id":"a/b","client_id":"a","client_secret": "***"}`,
		`{"tenant_id":"..","client_id":"a","client_secret": "***"}`,
		`{"tenant_id":"t","client_id":"a","client_secret": "***","token_uri":"http://evil.example/token"}`,
		`{"tenant_id":"t","client_id":"a","client_secret": "***","token_uri":"ftp://x/token"}`,
		`{"tenant_id":"t","client_id":"a","client_secret": "***","scope":"http://x/.default"}`,
		`{"tenant_id":"t","client_id":"a","client_secret": "***","scope":"https://x/other"}`,
	}
	for i, key := range bad {
		if _, err := parseEntraCredentials(key); err == nil {
			t.Errorf("bad credential %d accepted: %s", i, key)
		}
	}
	// http token_uri on loopback is allowed (test mocks).
	if _, err := parseEntraCredentials(`{"tenant_id":"t","client_id":"a","client_secret": "***","token_uri":"http://127.0.0.1:9/token"}`); err != nil {
		t.Errorf("loopback http token_uri rejected: %v", err)
	}
	// Sovereign-cloud scope override is allowed.
	if _, err := parseEntraCredentials(`{"tenant_id":"t","client_id":"a","client_secret": "***","scope":"https://cognitiveservices.azure.us/.default"}`); err != nil {
		t.Errorf("valid scope override rejected: %v", err)
	}
}

func TestEntraKeyFingerprintRotation(t *testing.T) {
	a, _ := parseEntraCredentials(`{"tenant_id":"t","client_id":"a","client_secret": "rotation-secret-one"}`)
	b, _ := parseEntraCredentials(`{"tenant_id":"t","client_id":"a","client_secret": "rotation-secret-two"}`)
	if entraKeyFingerprint(a) == entraKeyFingerprint(b) {
		t.Error("rotated secret kept the same fingerprint")
	}
	if strings.Contains(entraKeyFingerprint(a), "rotation-secret-one") {
		t.Error("fingerprint carries secret material")
	}
}

func TestEntraMintFormScopeAndCache(t *testing.T) {
	m := newEntraTokenMock(t)
	c, err := parseEntraCredentials(entraTestCred(m.srv.URL, "mint-form-test"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tok, err := entraAccessToken(context.Background(), c, m.srv.Client())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok != "entra-token-1" {
		t.Errorf("token = %q", tok)
	}
	if m.lastForm.Get("grant_type") != "client_credentials" ||
		m.lastForm.Get("client_id") != "mint-form-test" ||
		m.lastForm.Get("client_secret") != "test-secret-value" ||
		m.lastForm.Get("scope") != entraDefaultScope {
		t.Errorf("mint form = %v", m.lastForm)
	}
	// Second call is a cache hit: no second HTTP mint.
	tok2, err := entraAccessToken(context.Background(), c, m.srv.Client())
	if err != nil || tok2 != tok {
		t.Fatalf("cached token: %q %v", tok2, err)
	}
	if m.calls.Load() != 1 {
		t.Errorf("mint calls = %d, want 1 (cache hit)", m.calls.Load())
	}
	mints, hits, keys := EntraTokenCacheStats()
	if mints < 1 || hits < 1 || keys < 1 {
		t.Errorf("cache stats = mints %d hits %d keys %d", mints, hits, keys)
	}
}

func TestEntraTokenExpiryRemint(t *testing.T) {
	m := newEntraTokenMock(t)
	c, _ := parseEntraCredentials(entraTestCred(m.srv.URL, "expiry-test"))
	if _, err := entraAccessToken(context.Background(), c, m.srv.Client()); err != nil {
		t.Fatalf("first mint: %v", err)
	}
	// Force the cached token past its expiry: the next call must
	// re-mint (client-credentials has no refresh token).
	e := entraTokenCacheEntryFor(entraKeyFingerprint(c))
	e.mu.Lock()
	e.expiry = time.Now().Add(-time.Second)
	e.mu.Unlock()
	tok, err := entraAccessToken(context.Background(), c, m.srv.Client())
	if err != nil {
		t.Fatalf("re-mint: %v", err)
	}
	if tok != "entra-token-2" || m.calls.Load() != 2 {
		t.Errorf("re-mint token = %q, calls = %d, want entra-token-2 / 2", tok, m.calls.Load())
	}
}

func TestEntraMintBreaker(t *testing.T) {
	m := newEntraTokenMock(t)
	m.status, m.body = 401, `{"error":"invalid_client"}`
	oldF, oldC := entraMintBreakerFailures, entraMintBreakerCooldown
	entraMintBreakerFailures, entraMintBreakerCooldown = 2, 80*time.Millisecond
	defer func() { entraMintBreakerFailures, entraMintBreakerCooldown = oldF, oldC }()

	c, _ := parseEntraCredentials(entraTestCred(m.srv.URL, "breaker-test"))
	for i := 0; i < 2; i++ {
		if _, err := entraAccessToken(context.Background(), c, m.srv.Client()); err == nil {
			t.Fatalf("mint %d against a 401 endpoint succeeded", i)
		}
	}
	_, err := entraAccessToken(context.Background(), c, m.srv.Client())
	if err == nil || !strings.Contains(err.Error(), "cooling down") {
		t.Fatalf("breaker err = %v, want a cooldown refusal", err)
	}
	if m.calls.Load() != 2 {
		t.Errorf("breaker still hammered the endpoint: %d calls", m.calls.Load())
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := entraAccessToken(context.Background(), c, m.srv.Client()); err == nil {
		t.Fatal("mint after cooldown should be attempted again (and fail 401)")
	}
	if m.calls.Load() != 3 {
		t.Errorf("post-cooldown calls = %d, want 3", m.calls.Load())
	}
}

func TestEntraMintErrorNoSecretLeak(t *testing.T) {
	m := newEntraTokenMock(t)
	m.status = 400
	m.body = `{"error":"invalid_request","error_description":"the secret test-secret-value was echoed"}`
	c, _ := parseEntraCredentials(entraTestCred(m.srv.URL, "leak-test"))
	_, err := entraAccessToken(context.Background(), c, m.srv.Client())
	if err == nil {
		t.Fatal("400 mint succeeded")
	}
	if strings.Contains(err.Error(), "test-secret-value") {
		t.Errorf("mint error leaks the client secret: %v", err)
	}
	if !strings.Contains(err.Error(), "invalid_request") || !strings.Contains(err.Error(), "400") {
		t.Errorf("mint error should name the status and AAD code: %v", err)
	}
}

// TestRegistryAddCatalogAzureEntra is the end-to-end Entra path:
// add the catalog provider with client-credential JSON, the add-time
// key test mints a token and discovers models, and chat carries the
// minted Bearer token (never the raw credential JSON, never api-key).
func TestRegistryAddCatalogAzureEntra(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	tm := newEntraTokenMock(t)
	reg, _ := newCatalogTestRegistry(t)

	cred := entraTestCred(tm.srv.URL, "azure-e2e")
	p, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", cred,
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog azure-openai/entra: %v", err)
	}
	if p.WireFormat != WireFormatOpenAI || p.AuthEndpoint != "chat" || p.CatalogID != "azure-openai" {
		t.Errorf("provider facts = wire %q endpoint %q catalog %q", p.WireFormat, p.AuthEndpoint, p.CatalogID)
	}
	if len(p.Models) == 0 {
		t.Error("add-time model discovery found no models")
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	got, ok := mock.lastHeader("/v1/chat/completions", "Authorization")
	if !ok || got != "Bearer entra-token-1" {
		t.Errorf("Authorization = %q (present=%v), want the minted Bearer token", got, ok)
	}
	if _, ok := mock.lastHeader("/v1/chat/completions", "api-key"); ok {
		t.Error("Entra surface must not send an api-key header")
	}
	// The stored credential is the sealed JSON, and the registry
	// never exposes it: List returns the encrypted form only.
	for _, listed := range reg.List() {
		if strings.Contains(listed.APIKeyEncrypted, "test-secret-value") {
			t.Error("provider list carries plaintext credential material")
		}
	}
}

// TestRegistryAzureEntraAuthCacheExpiry proves the auth cache
// re-mints once the cached Bearer token expires: a long-lived
// provider never serves an expired token.
func TestRegistryAzureEntraAuthCacheExpiry(t *testing.T) {
	srv, mock := newRecordingServer(t)
	defer srv.Close()
	tm := newEntraTokenMock(t)
	reg, _ := newCatalogTestRegistry(t)

	cred := entraTestCred(tm.srv.URL, "azure-cache-expiry")
	p, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", cred,
		CatalogAddOptions{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("AddCatalog: %v", err)
	}
	// Force the minted token past expiry.
	c, _ := parseEntraCredentials(cred)
	e := entraTokenCacheEntryFor(entraKeyFingerprint(c))
	e.mu.Lock()
	e.expiry = time.Now().Add(-time.Second)
	e.mu.Unlock()

	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor after expiry: %v", err)
	}
	if _, err := client.Chat(context.Background(), ChatRequest{Model: "model-a", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("Chat after expiry: %v", err)
	}
	if tm.calls.Load() < 2 {
		t.Errorf("token mints = %d, want a re-mint after expiry", tm.calls.Load())
	}
	got, _ := mock.lastHeader("/v1/chat/completions", "Authorization")
	if got != "Bearer entra-token-2" {
		t.Errorf("Authorization after expiry = %q, want the re-minted token", got)
	}
}

func TestRegistryAzureEntraRefusals(t *testing.T) {
	srv, _ := newRecordingServer(t)
	defer srv.Close()
	reg, _ := newCatalogTestRegistry(t)

	t.Run("non-entra key keeps exchange refusal", func(t *testing.T) {
		_, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", "k",
			CatalogAddOptions{BaseURL: srv.URL})
		if err == nil || !strings.Contains(err.Error(), "exchange") {
			t.Fatalf("err = %v, want the exchange refusal", err)
		}
	})
	t.Run("mint failure surfaces honestly", func(t *testing.T) {
		tm := newEntraTokenMock(t)
		tm.status, tm.body = 401, `{"error":"invalid_client"}`
		_, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai",
			entraTestCred(tm.srv.URL, "azure-mint-fail"),
			CatalogAddOptions{BaseURL: srv.URL})
		if err == nil {
			t.Fatal("add with a rejected token mint succeeded")
		}
		if !strings.Contains(err.Error(), "entra:") || !strings.Contains(err.Error(), "401") {
			t.Errorf("err = %v, want the honest mint failure (entra / 401)", err)
		}
		if strings.Contains(err.Error(), "test-secret-value") {
			t.Error("add error leaks the client secret")
		}
	})
	t.Run("api-key variant unaffected", func(t *testing.T) {
		p, err := reg.AddCatalogWithOptions(context.Background(), "azure-openai", "az-static-key",
			CatalogAddOptions{Endpoint: "chat", Variant: "api-key", BaseURL: srv.URL})
		if err != nil {
			t.Fatalf("api-key variant: %v", err)
		}
		if p.AuthVariant != "api-key" {
			t.Errorf("variant = %q", p.AuthVariant)
		}
	})
}
