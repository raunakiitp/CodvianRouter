// Vertex AI provider client tests (CHECKLIST 277 (e)): the
// service-account JSON exchange path — a real RS256 JWT minted
// from a freshly generated RSA key, exchanged at a mock token
// endpoint, then a minted Bearer token on a mock Vertex API.
// Nothing here talks to Google: the token endpoint and the Vertex
// API are both httptest servers, and the test proves the exact
// requests the real Google endpoints would see (grant type,
// assertion, Bearer token, project/location path).
package provider

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// vertexMock serves the Vertex AI Gemini surface the way the real
// API does (GET .../publishers/google/models, POST
// .../publishers/google/models/{model}:generateContent and
// :streamGenerateContent?alt=sse) and records every request, so
// tests can prove the request shape, the minted auth header and
// the project/location path.
type vertexMock struct {
	t           *testing.T
	lastPath    string
	lastQuery   string
	lastHeaders http.Header
	lastBody    []byte
	streamBody  string
	errStatus   int
	errBody     string
}

func (m *vertexMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.lastPath = r.URL.Path
		m.lastQuery = r.URL.RawQuery
		m.lastHeaders = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		m.lastBody = body
		switch {
		case strings.HasSuffix(r.URL.Path, "/publishers/google/models") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[{"name":"projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.0-flash"},{"name":"projects/proj-1/locations/us-central1/publishers/google/models/gemini-1.5-pro"}]}`))
		case strings.HasSuffix(r.URL.Path, ":generateContent"):
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi there"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20},"modelVersion":"gemini-2.0-flash-001"}`))
		case strings.HasSuffix(r.URL.Path, ":streamGenerateContent"):
			if m.errStatus != 0 {
				w.WriteHeader(m.errStatus)
				_, _ = w.Write([]byte(m.errBody))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(m.streamBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func newVertexMock(t *testing.T) (*httptest.Server, *vertexMock) {
	t.Helper()
	m := &vertexMock{t: t, streamBody: "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hi \"}]},\"finishReason\":\"STOP\"}]}\n\ndata: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"there\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":12,\"candidatesTokenCount\":8,\"totalTokenCount\":20}}\n\n"}
	return httptest.NewServer(m.handler()), m
}

// vertexTokenMock is a mock OAuth2 token endpoint: it requires the
// JWT-bearer grant type and a non-empty assertion, and answers
// with a fixed access token. hits counts exchanges.
type vertexTokenMock struct {
	hits       atomic.Int32
	lastGrant  string
	lastAssert string
	errStatus  int
}

func (m *vertexTokenMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		// Parse the raw body: ReadAll already drained it, so
		// ParseForm would see nothing.
		form, _ := url.ParseQuery(string(body))
		m.lastGrant = form.Get("grant_type")
		m.lastAssert = form.Get("assertion")
		if m.errStatus != 0 {
			w.WriteHeader(m.errStatus)
			_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad assertion"}`))
			return
		}
		if m.lastGrant != "urn:ietf:params:oauth:grant-type:jwt-bearer" || m.lastAssert == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"tok-vertex-abc","expires_in":3600,"token_type":"Bearer"}`))
	}
}

// testServiceAccountJSON builds a service-account JSON key for
// tests: a real 2048-bit RSA key, PEM-encoded, with token_uri
// pointing at the mock token server.
func testServiceAccountJSON(t *testing.T, tokenURL string) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"type":         "service-account",
		"project_id":   "proj-1",
		"client_email": "vertex-test@proj-1.iam.gserviceaccount.com",
		"private_key":  string(pemKey),
		"token_uri":    tokenURL,
	}
	b, err := json.Marshal(sa)
	if err != nil {
		t.Fatalf("marshal service-account JSON: %v", err)
	}
	return string(b)
}

// vertexTestServers wires a token mock + a Vertex mock and returns
// the client pointed at the Vertex mock's concrete base URL
// (.../v1/projects/proj-1/locations/us-central1).
func vertexTestServers(t *testing.T) (*httptest.Server, *vertexMock, *vertexTokenMock, *vertexClient) {
	t.Helper()
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	t.Cleanup(tokSrv.Close)
	srv, mock := newVertexMock(t)
	t.Cleanup(srv.Close)
	base := srv.URL + "/v1/projects/proj-1/locations/us-central1"
	c, err := NewClient(base, testServiceAccountJSON(t, tokSrv.URL), WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient vertex: %v", err)
	}
	vc, ok := c.(*vertexClient)
	if !ok {
		t.Fatalf("NewClient with vertex wire returned %T, want *vertexClient", c)
	}
	return srv, mock, tok, vc
}

func TestVertexChatMintsTokenAndRoutes(t *testing.T) {
	_, mock, tok, client := vertexTestServers(t)
	resp, err := client.Chat(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hi there" {
		t.Fatalf("content = %q, want %q", resp.Content, "hi there")
	}
	// The token exchange ran exactly once, with the JWT-bearer
	// grant and a real (3-part) assertion — not a faked header.
	if got := tok.hits.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1", got)
	}
	if parts := strings.Split(tok.lastAssert, "."); len(parts) != 3 {
		t.Fatalf("assertion has %d parts, want a 3-part JWT", len(parts))
	}
	// The Vertex request carries the minted token and the
	// project/location path from the operator's base URL.
	if got := mock.lastHeaders.Get("Authorization"); got != "Bearer tok-vertex-abc" {
		t.Fatalf("Authorization = %q, want the minted Bearer token", got)
	}
	if want := "/v1/projects/proj-1/locations/us-central1/publishers/google/models/gemini-2.0-flash:generateContent"; mock.lastPath != want {
		t.Fatalf("path = %q, want %q", mock.lastPath, want)
	}
}

func TestVertexTokenCacheSharedAcrossClients(t *testing.T) {
	// CHECKLIST 283 (a): two clients built from the SAME
	// service-account key share one minted token — one exchange
	// for two chats, not two. The first chat mints; the second
	// client's chat reuses the cached token.
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	t.Cleanup(tokSrv.Close)
	srv, _ := newVertexMock(t)
	t.Cleanup(srv.Close)
	saJSON := testServiceAccountJSON(t, tokSrv.URL)
	base := srv.URL + "/v1/projects/proj-1/locations/us-central1"
	newClient := func(t *testing.T) *vertexClient {
		t.Helper()
		c, err := NewClient(base, saJSON, WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
		if err != nil {
			t.Fatalf("NewClient vertex: %v", err)
		}
		vc, ok := c.(*vertexClient)
		if !ok {
			t.Fatalf("NewClient with vertex wire returned %T, want *vertexClient", c)
		}
		return vc
	}
	mintsBefore, hitsBefore, _ := VertexTokenCacheStats()
	req := ChatRequest{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}}}
	if _, err := newClient(t).Chat(context.Background(), req); err != nil {
		t.Fatalf("first client Chat: %v", err)
	}
	if _, err := newClient(t).Chat(context.Background(), req); err != nil {
		t.Fatalf("second client Chat: %v", err)
	}
	if got := tok.hits.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1 (two clients, same service-account key, mint once)", got)
	}
	mints, hits, keys := VertexTokenCacheStats()
	if mints-mintsBefore != 1 {
		t.Fatalf("cache mints delta = %d, want 1", mints-mintsBefore)
	}
	if hits-hitsBefore != 1 {
		t.Fatalf("cache hits delta = %d, want 1 (the second chat reused the token)", hits-hitsBefore)
	}
	if keys < 1 {
		t.Fatalf("cache keys = %d, want >= 1", keys)
	}
}

func TestVertexTokenCacheSeparatePerKey(t *testing.T) {
	// Sensitivity for the sharing test above: two clients built
	// from DIFFERENT service-account keys must mint separately —
	// the cache is keyed by the key, not shared across keys.
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	t.Cleanup(tokSrv.Close)
	srv, _ := newVertexMock(t)
	t.Cleanup(srv.Close)
	base := srv.URL + "/v1/projects/proj-1/locations/us-central1"
	req := ChatRequest{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}}}
	for i := 0; i < 2; i++ {
		c, err := NewClient(base, testServiceAccountJSON(t, tokSrv.URL), WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
		if err != nil {
			t.Fatalf("NewClient %d: %v", i, err)
		}
		if _, err := c.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
	}
	if got := tok.hits.Load(); got != 2 {
		t.Fatalf("token exchanges = %d, want 2 (different service-account keys mint separately)", got)
	}
}

func TestVertexTokenCachedAcrossCalls(t *testing.T) {
	_, _, tok, client := vertexTestServers(t)
	ctx := context.Background()
	req := ChatRequest{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}}}
	for i := 0; i < 2; i++ {
		if _, err := client.Chat(ctx, req); err != nil {
			t.Fatalf("Chat %d: %v", i, err)
		}
	}
	if got := tok.hits.Load(); got != 1 {
		t.Fatalf("token exchanges = %d, want 1 (the token is cached ~1h, not minted per request)", got)
	}
}

func TestVertexChatStream(t *testing.T) {
	_, mock, _, client := vertexTestServers(t)
	var got string
	resp, err := client.ChatStreamChunks(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	}, func(chunk StreamChunk) error {
		got += chunk.Content
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if got != "hi there" || resp.Content != "hi there" {
		t.Fatalf("streamed = %q, reassembled = %q, want %q", got, resp.Content, "hi there")
	}
	if !strings.HasSuffix(mock.lastPath, ":streamGenerateContent") {
		t.Fatalf("path = %q, want the streamGenerateContent endpoint", mock.lastPath)
	}
	if mock.lastQuery != "alt=sse" {
		t.Fatalf("query = %q, want alt=sse", mock.lastQuery)
	}
}

func TestVertexListModelsStripsPublisherPrefix(t *testing.T) {
	_, _, _, client := vertexTestServers(t)
	ids, err := client.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != "gemini-2.0-flash" || ids[1] != "gemini-1.5-pro" {
		t.Fatalf("models = %v, want [gemini-2.0-flash gemini-1.5-pro]", ids)
	}
}

func TestVertexClientOverrideRoutes(t *testing.T) {
	_, mock, _, client := vertexTestServers(t)
	// Location override: the request lands in europe-west1.
	_, err := client.Chat(context.Background(), ChatRequest{
		Model:                "gemini-2.0-flash",
		Messages:             []Message{{Role: "user", Content: "Hi"}},
		ClientVertexOverride: &VertexOverride{Field: "vertex_location", Value: "europe-west1"},
	})
	if err != nil {
		t.Fatalf("Chat with location override: %v", err)
	}
	if !strings.Contains(mock.lastPath, "/locations/europe-west1/") {
		t.Fatalf("path = %q, want the overridden location europe-west1", mock.lastPath)
	}
	if !strings.Contains(mock.lastPath, "/projects/proj-1/") {
		t.Fatalf("path = %q, want the operator's project untouched", mock.lastPath)
	}
	// Project override: the request lands in another project.
	_, err = client.Chat(context.Background(), ChatRequest{
		Model:                "gemini-2.0-flash",
		Messages:             []Message{{Role: "user", Content: "Hi"}},
		ClientVertexOverride: &VertexOverride{Field: "vertex_project", Value: "proj-2"},
	})
	if err != nil {
		t.Fatalf("Chat with project override: %v", err)
	}
	if !strings.Contains(mock.lastPath, "/projects/proj-2/") {
		t.Fatalf("path = %q, want the overridden project proj-2", mock.lastPath)
	}
}

func TestVertexMissingCredentialsFailLoudly(t *testing.T) {
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	defer tokSrv.Close()
	srv, _ := newVertexMock(t)
	defer srv.Close()
	base := srv.URL + "/v1/projects/proj-1/locations/us-central1"
	cases := []struct {
		name string
		key  string
		want string
	}{
		{"not JSON at all", "not-a-service-account", "service-account JSON"},
		{"empty JSON object", "{}", "client_email"},
		{"wrong credential type", `{"type":"authorized_user","client_email":"a@b.c"}`, "not a service-account JSON"},
		{"unparseable private key", `{"type":"service-account","client_email":"a@b.c","private_key":"not-pem","token_uri":"` + tokSrv.URL + `"}`, "private_key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(base, tc.key, WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			_, err = c.Chat(context.Background(), ChatRequest{
				Model:    "gemini-2.0-flash",
				Messages: []Message{{Role: "user", Content: "Hi"}},
			})
			if err == nil {
				t.Fatal("Chat with bad credentials: expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to name %q", err.Error(), tc.want)
			}
			if got := tok.hits.Load(); got != 0 {
				t.Fatalf("token exchanges = %d, want 0 (a bad key must never reach the exchange)", got)
			}
		})
	}
}

func TestVertexTokenExchangeRejectionFailsLoudly(t *testing.T) {
	tok := &vertexTokenMock{errStatus: http.StatusBadRequest}
	tokSrv := httptest.NewServer(tok.handler())
	defer tokSrv.Close()
	srv, mock := newVertexMock(t)
	defer srv.Close()
	c, err := NewClient(srv.URL+"/v1/projects/proj-1/locations/us-central1",
		testServiceAccountJSON(t, tokSrv.URL), WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("Chat with a rejected exchange: err = %v, want a token-exchange error", err)
	}
	if mock.lastPath != "" {
		t.Fatalf("Vertex API was called (%q) despite the failed exchange — no request may go out unauthenticated", mock.lastPath)
	}
}

func TestVertexBaseURLWithoutProjectFailsLoudly(t *testing.T) {
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	defer tokSrv.Close()
	srv, _ := newVertexMock(t)
	defer srv.Close()
	// A bare host (or a /v1 root) names no project/location to
	// bill — the client must fail naming the expected shape,
	// never guess a project.
	c, err := NewClient(srv.URL+"/v1", testServiceAccountJSON(t, tokSrv.URL), WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.Chat(context.Background(), ChatRequest{
		Model:    "gemini-2.0-flash",
		Messages: []Message{{Role: "user", Content: "Hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "projects/{project}/locations/{location}") {
		t.Fatalf("Chat with a project-less base URL: err = %v, want the expected-shape error", err)
	}
}

// TestVertexRegistryAddCatalog proves the registry end of the
// exchange: a catalog Vertex add with a real service-account JSON
// key and a concrete base URL succeeds and builds a Vertex
// client, while the existing refusal tests (a non-JSON key) stay
// refused. The mock token + Vertex servers stand in for Google.
func TestVertexRegistryAddCatalog(t *testing.T) {
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	defer tokSrv.Close()
	srv, mock := newVertexMock(t)
	defer srv.Close()
	reg := NewRegistry(nil, WithRegistryAllowPrivateDestinations(true))
	p, err := reg.AddCatalogWithOptions(context.Background(), "vertex",
		testServiceAccountJSON(t, tokSrv.URL),
		CatalogAddOptions{BaseURL: srv.URL + "/v1/projects/proj-1/locations/us-central1"})
	if err != nil {
		t.Fatalf("AddCatalog vertex with service-account JSON: %v", err)
	}
	if p.WireFormat != WireFormatVertex {
		t.Fatalf("provider wire = %q, want %q", p.WireFormat, WireFormatVertex)
	}
	client, err := reg.ClientFor(p.ID)
	if err != nil {
		t.Fatalf("ClientFor vertex: %v", err)
	}
	if _, ok := client.(*vertexClient); !ok {
		t.Fatalf("ClientFor returned %T, want *vertexClient", client)
	}
	if err := client.TestKey(context.Background()); err != nil {
		t.Fatalf("TestKey: %v", err)
	}
	if mock.lastPath != "/v1/projects/proj-1/locations/us-central1/publishers/google/models" {
		t.Fatalf("TestKey path = %q, want the Vertex ListModels path", mock.lastPath)
	}
	if got := mock.lastHeaders.Get("Authorization"); got != "Bearer tok-vertex-abc" {
		t.Fatalf("TestKey Authorization = %q, want the minted Bearer token", got)
	}
	// Sensitivity: the same add with a non-JSON key stays
	// refused (the existing TestRegistryAddCatalogRefusals case
	// pins this too).
	if _, err := reg.AddCatalogWithOptions(context.Background(), "vertex", "k",
		CatalogAddOptions{BaseURL: srv.URL + "/v1/projects/proj-1/locations/us-central1"}); err == nil {
		t.Fatal("AddCatalog vertex with a non-JSON key: expected an exchange refusal, got nil")
	} else if !strings.Contains(err.Error(), "exchange") {
		t.Fatalf("error = %q, want it to name the exchange", err.Error())
	}
}

// vertexTestCacheCleanup removes the named fingerprints from the
// shared token cache at test end, so an eviction test's slots do
// not leak into other tests' stats.
func vertexTestCacheCleanup(t *testing.T, fps ...string) {
	t.Helper()
	t.Cleanup(func() {
		vertexTokenCacheMu.Lock()
		defer vertexTokenCacheMu.Unlock()
		for _, fp := range fps {
			delete(vertexTokenCache, fp)
		}
	})
}

// TestVertexTokenCacheEvictsExpiredEntries (CHECKLIST 310): two
// keys mint once each; the first key's token is then expired, and
// the sweep reclaims exactly that key's map slot — the live key's
// entry (and its token) stays cached, proven by a second chat for
// the live key minting nothing new.
func TestVertexTokenCacheEvictsExpiredEntries(t *testing.T) {
	tok := &vertexTokenMock{}
	tokSrv := httptest.NewServer(tok.handler())
	t.Cleanup(tokSrv.Close)
	srv, _ := newVertexMock(t)
	t.Cleanup(srv.Close)
	base := srv.URL + "/v1/projects/proj-1/locations/us-central1"
	req := ChatRequest{Model: "gemini-2.0-flash", Messages: []Message{{Role: "user", Content: "Hi"}}}
	newClient := func(t *testing.T, saJSON string) *vertexClient {
		t.Helper()
		c, err := NewClient(base, saJSON, WithWireFormat(WireFormatVertex), WithAllowPrivateDestinations(true))
		if err != nil {
			t.Fatalf("NewClient vertex: %v", err)
		}
		vc, ok := c.(*vertexClient)
		if !ok {
			t.Fatalf("NewClient with vertex wire returned %T, want *vertexClient", c)
		}
		return vc
	}
	saA := testServiceAccountJSON(t, tokSrv.URL)
	saB := testServiceAccountJSON(t, tokSrv.URL)
	saParsedA, _, err := parseVertexServiceAccount(saA)
	if err != nil {
		t.Fatalf("parse SA A: %v", err)
	}
	saParsedB, _, err := parseVertexServiceAccount(saB)
	if err != nil {
		t.Fatalf("parse SA B: %v", err)
	}
	fpA := vertexKeyFingerprint(saParsedA)
	fpB := vertexKeyFingerprint(saParsedB)
	vertexTestCacheCleanup(t, fpA, fpB)
	clientA := newClient(t, saA)
	clientB := newClient(t, saB)
	if _, err := clientA.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat A: %v", err)
	}
	if _, err := clientB.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat B: %v", err)
	}
	if got := tok.hits.Load(); got != 2 {
		t.Fatalf("token exchanges = %d, want 2 (one mint per key)", got)
	}
	_, _, keysBefore := VertexTokenCacheStats()
	entryA := vertexTokenCacheEntryFor(fpA)
	entryB := vertexTokenCacheEntryFor(fpB)
	if entryA == entryB {
		t.Fatal("entries for two keys are the same pointer, want separate slots")
	}
	// Expire A's token in place (same-package test reaching the
	// entry fields, as a token past its hour would be).
	entryA.mu.Lock()
	entryA.expiry = time.Now().Add(-time.Minute)
	entryA.mu.Unlock()
	// The sweep reclaims exactly A's slot (plus, in principle, a
	// stale token-less slot another test left behind — the count
	// below is exact about the keys total, not about the pruned
	// count, so a stale leftover cannot make this flaky).
	pruned := vertexTokenCachePruneExpired(time.Now())
	if pruned < 1 {
		t.Fatalf("pruned = %d, want >= 1 (the expired key's slot)", pruned)
	}
	_, _, keysAfter := VertexTokenCacheStats()
	if keysAfter != keysBefore-pruned {
		t.Fatalf("cache keys = %d, want %d (one slot reclaimed per pruned entry)", keysAfter, keysBefore-pruned)
	}
	// A's slot is gone: a fresh lookup creates a new, empty entry.
	if fresh := vertexTokenCacheEntryFor(fpA); fresh == entryA || fresh.token != "" {
		t.Fatalf("fresh entry for A = %p (token %q), want a new empty entry, not the expired one", fresh, fresh.token)
	}
	// B's slot is untouched: same entry, same live token.
	if kept := vertexTokenCacheEntryFor(fpB); kept != entryB || kept.token != "tok-vertex-abc" {
		t.Fatalf("entry for B = %p (token %q), want the original entry with its live token", kept, kept.token)
	}
	// And the live key's token stays cached in practice: a second
	// chat for B mints nothing new.
	if _, err := clientB.Chat(context.Background(), req); err != nil {
		t.Fatalf("second Chat B: %v", err)
	}
	if got := tok.hits.Load(); got != 2 {
		t.Fatalf("token exchanges = %d, want 2 (the live key's token stayed cached after the sweep)", got)
	}
}

// TestVertexTokenCacheCapBoundsMap (CHECKLIST 310): with the cap
// lowered to 3, inserting a fifth distinct key evicts the slots
// whose tokens expire soonest, and the map never grows past the
// cap. The kept test slots are the newest ones: earlier tests in
// this package leave live entries behind (all older than these,
// so they are evicted first) and may leave a fresh token-less
// entry behind (a failed mint's slot, still inside its grace
// period — it is never the eviction victim, so it occupies one
// of the 3 slots). The test measures that leftover count up front
// and asserts the kept set relative to it, so it does not depend
// on how many entries other tests left.
func TestVertexTokenCacheCapBoundsMap(t *testing.T) {
	oldCap := vertexTokenCacheMaxEntries
	vertexTokenCacheMaxEntries = 3
	t.Cleanup(func() { vertexTokenCacheMaxEntries = oldCap })
	fps := []string{"cap-test-0", "cap-test-1", "cap-test-2", "cap-test-3", "cap-test-4"}
	vertexTestCacheCleanup(t, fps...)
	// Fresh token-less entries left by earlier tests (a failed
	// mint's slot, still inside its grace period) occupy cap
	// slots without ever being eviction victims.
	vertexTokenCacheMu.Lock()
	tokenlessLeftovers := 0
	for _, e := range vertexTokenCache {
		e.mu.Lock()
		if e.token == "" {
			tokenlessLeftovers++
		}
		e.mu.Unlock()
	}
	vertexTokenCacheMu.Unlock()
	// The test slots that survive are the newest
	// (cap - leftovers) of the five inserts.
	keep := 3 - tokenlessLeftovers
	if keep < 1 {
		t.Fatalf("token-less leftovers = %d, want < 3 (no room left for a live test slot)", tokenlessLeftovers)
	}
	now := time.Now()
	// Five live slots with distinct expiries, oldest first.
	for i := 0; i < 5; i++ {
		e := vertexTokenCacheEntryFor(fps[i])
		e.mu.Lock()
		e.token = "tok-live"
		e.expiry = now.Add(time.Duration(i+1) * time.Hour)
		e.mu.Unlock()
	}
	// Exactly the newest `keep` test slots survive; every older
	// one was evicted, oldest first.
	for i, fp := range fps {
		vertexTokenCacheMu.Lock()
		_, kept := vertexTokenCache[fp]
		vertexTokenCacheMu.Unlock()
		wantKept := i >= len(fps)-keep
		if kept != wantKept {
			t.Errorf("slot %s kept = %v, want %v (kept set must be the newest %d of the five)", fp, kept, wantKept, keep)
		}
	}
	// The oldest test slot is always gone, whatever the leftovers.
	vertexTokenCacheMu.Lock()
	_, gone0 := vertexTokenCache[fps[0]]
	vertexTokenCacheMu.Unlock()
	if gone0 {
		t.Errorf("cap-test-0 still cached, want it evicted (soonest expiry)")
	}
	// A token-less entry inside its grace period (a first mint in
	// flight) is never the eviction victim: insert two more keys
	// with no token, and the first must survive even past the cap.
	fp5, fp6 := "cap-test-5", "cap-test-6"
	vertexTestCacheCleanup(t, fp5, fp6)
	_ = vertexTokenCacheEntryFor(fp5) // no token, no expiry
	_ = vertexTokenCacheEntryFor(fp6)
	vertexTokenCacheMu.Lock()
	_, kept5 := vertexTokenCache[fp5]
	vertexTokenCacheMu.Unlock()
	if !kept5 {
		t.Errorf("token-less cap-test-5 evicted, want it kept (a first mint may be in flight)")
	}
}

// TestVertexTokenCachePruneReapsStaleTokenlessEntries (CHECKLIST
// 310): a token-less entry older than the grace period — a mint
// that failed or a client that went away — is reclaimed by the
// sweep, so a key whose mint always fails cannot hold a slot
// forever; a fresh token-less entry (a first mint in flight) is
// not touched.
func TestVertexTokenCachePruneReapsStaleTokenlessEntries(t *testing.T) {
	staleFP, freshFP := "cap-stale-0", "cap-fresh-0"
	vertexTestCacheCleanup(t, staleFP, freshFP)
	stale := vertexTokenCacheEntryFor(staleFP)
	stale.mu.Lock()
	stale.created = time.Now().Add(-(vertexTokenCacheTokenlessGrace + time.Minute))
	stale.mu.Unlock()
	_ = vertexTokenCacheEntryFor(freshFP) // fresh, token-less, in grace
	if pruned := vertexTokenCachePruneExpired(time.Now()); pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the stale token-less slot)", pruned)
	}
	vertexTokenCacheMu.Lock()
	_, staleGone := vertexTokenCache[staleFP]
	_, freshKept := vertexTokenCache[freshFP]
	vertexTokenCacheMu.Unlock()
	if staleGone {
		t.Errorf("stale token-less entry still cached, want it reclaimed")
	}
	if !freshKept {
		t.Errorf("fresh token-less entry pruned, want it kept (a first mint may be in flight)")
	}
}
