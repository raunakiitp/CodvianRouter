package server

// oauth_conn_guard_test.go — audit finding AUD-SEC-005.
//
// The provider OAuth token exchange must go through the SSRF-guarded
// egress client (internal/egress), never http.DefaultClient. These tests
// pin the three things that matter:
//
//  1. the exchange uses the store's client (so the call site cannot
//     silently drift back to a default client);
//  2. the default client IS the guarded one (no proxy, validated dial);
//  3. a provider-controlled loopback token endpoint is refused, and the
//     connection is not stored.
//
// Plus a source-level guard so no production file in this package can
// reintroduce an unguarded outbound http.DefaultClient call.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// spyTransport counts round trips and answers a valid token response.
type spyTransport struct {
	calls atomic.Int64
	body  string
}

func (s *spyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	body := s.body
	if body == "" {
		body = `{"access_token":"tok","refresh_token":"ref","token_type":"Bearer","expires_in":3600,"scope":"read"}`
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// withTempProvider registers a throwaway provider whose token endpoint is
// tokenURL, with client credentials in the environment, and restores the
// registry afterwards.
func withTempProvider(t *testing.T, id, tokenURL string) {
	t.Helper()
	if _, exists := oauthProviders[id]; exists {
		t.Fatalf("provider %q already registered", id)
	}
	oauthProviders[id] = oauthProvider{ID: id, Name: id, AuthURL: tokenURL, TokenURL: tokenURL}
	up := strings.ToUpper(id)
	t.Setenv("CODVIAN_OAUTH_"+up+"_CLIENT_ID", "cid")
	t.Setenv("CODVIAN_OAUTH_"+up+"_CLIENT_SECRET", "csecret")
	t.Cleanup(func() { delete(oauthProviders, id) })
}

// TestOAuthTokenExchangeUsesStoreClient proves CompleteAuth performs the
// exchange with the client the store hands it (and therefore not with a
// hardcoded package-level default).
func TestOAuthTokenExchangeUsesStoreClient(t *testing.T) {
	withTempProvider(t, "testprov_spy", "https://example.invalid/token")

	spy := &spyTransport{}
	store := newOAuthConnStore(nil)
	store.client = &http.Client{Transport: spy, Timeout: 5 * time.Second}

	if err := store.CompleteAuth("testprov_spy", "code-123", "https://app.example/cb"); err != nil {
		t.Fatalf("CompleteAuth: %v", err)
	}
	if got := spy.calls.Load(); got != 1 {
		t.Fatalf("token exchange round trips = %d, want 1", got)
	}
	if tok, err := store.GetAccessToken("testprov_spy"); err != nil || tok != "tok" {
		t.Fatalf("stored access token = %q, err = %v; want \"tok\"", tok, err)
	}
}

// TestOAuthTokenExchangeSuccessPath stores a complete connection: the
// regression guard for the P1 behaviour that must keep working.
func TestOAuthTokenExchangeSuccessPath(t *testing.T) {
	withTempProvider(t, "testprov_ok", "https://example.invalid/token")
	store := newOAuthConnStore(nil)
	store.client = &http.Client{Transport: &spyTransport{}, Timeout: 5 * time.Second}

	if err := store.CompleteAuth("testprov_ok", "code", "https://app.example/cb"); err != nil {
		t.Fatalf("CompleteAuth: %v", err)
	}
	conns := store.ListConnections()
	if len(conns) != 1 || !conns[0].Connected || conns[0].Provider != "testprov_ok" {
		t.Fatalf("connections = %+v, want one connected testprov_ok", conns)
	}
	if conns[0].ExpiresAt.Before(time.Now()) {
		t.Fatalf("expiry %v is in the past despite expires_in=3600", conns[0].ExpiresAt)
	}
}

// TestOAuthTokenExchangeDefaultClientIsSSRFGuarded asserts the client the
// production path uses is the egress-guarded one and not a shared default.
func TestOAuthTokenExchangeDefaultClientIsSSRFGuarded(t *testing.T) {
	store := newOAuthConnStore(nil)
	c := store.oauthTokenClient()
	if c == nil {
		t.Fatal("oauthTokenClient() returned nil")
	}
	if c == http.DefaultClient {
		t.Fatal("oauthTokenClient() returned http.DefaultClient: the exchange would bypass the SSRF guard")
	}
	if c.Transport == nil || c.Transport == http.DefaultTransport {
		t.Fatalf("oauthTokenClient() transport = %T, want the egress transport (nil/default transport means no validated dialing)", c.Transport)
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("oauthTokenClient() transport type = %T, want *http.Transport", c.Transport)
	}
	if tr.DialContext == nil {
		t.Fatal("egress transport has no DialContext: destination validation would be skipped")
	}
	if tr.Proxy != nil {
		t.Fatal("egress transport must not use a proxy: a proxy resolves the host itself and defeats the guard")
	}
	if c.Timeout != 30*time.Second {
		t.Fatalf("token-exchange client timeout = %v, want 30s", c.Timeout)
	}
}

// TestOAuthTokenExchangeRefusesLoopbackTokenURL is the negative case: a
// provider whose token endpoint is a loopback address must be refused by
// the guard, and nothing may be stored.
func TestOAuthTokenExchangeRefusesLoopbackTokenURL(t *testing.T) {
	withTempProvider(t, "testprov_loop", "http://127.0.0.1:9/token")

	store := newOAuthConnStore(nil) // no injected client: the production path
	err := store.CompleteAuth("testprov_loop", "code", "https://app.example/cb")
	if err == nil {
		t.Fatal("CompleteAuth succeeded against a loopback token endpoint; the SSRF guard did not apply")
	}
	if !errors.Is(err, errEgressBlocked) {
		t.Fatalf("error = %v, want it to wrap the egress block error (a dial failure caused by something else would not prove the guard ran)", err)
	}
	if conns := store.ListConnections(); len(conns) != 0 {
		t.Fatalf("connections = %+v after a blocked exchange, want none", conns)
	}
}

// TestOAuthTokenExchangeRefusesMetadataAddress covers the cloud-metadata
// class of destination as well as loopback.
func TestOAuthTokenExchangeRefusesMetadataAddress(t *testing.T) {
	withTempProvider(t, "testprov_meta", "http://169.254.169.254/latest/meta-data/token")
	store := newOAuthConnStore(nil)
	err := store.CompleteAuth("testprov_meta", "code", "https://app.example/cb")
	if err == nil {
		t.Fatal("CompleteAuth succeeded against a link-local metadata address")
	}
	if !errors.Is(err, errEgressBlocked) {
		t.Fatalf("error = %v, want the egress block error", err)
	}
}

// TestNoUnguardedDefaultClientInServerProduction closes the class, not
// just the instance: no non-test file in this package may perform an
// outbound call with http.DefaultClient.
func TestNoUnguardedDefaultClientInServerProduction(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "http.DefaultClient.") && strings.Contains(line, "Do(") {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", f, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("unguarded outbound http.DefaultClient call(s) in production code:\n%s", strings.Join(offenders, "\n"))
	}
}
