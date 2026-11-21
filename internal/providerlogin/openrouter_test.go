package providerlogin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fixedRand is a deterministic randomness source: sequential bytes
// 0..255 repeating, so the verifier (first 32 bytes) and state (next
// 32 bytes) of a Start are known to the test.
type fixedRand struct{ n byte }

func (r *fixedRand) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

func fixedB64(start byte, n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = start + byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// TestCodeChallengeRFC7636Vector pins the S256 derivation to the
// RFC 7636 appendix-B test vector.
func TestCodeChallengeRFC7636Vector(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := CodeChallenge(verifier); got != want {
		t.Fatalf("CodeChallenge(RFC vector) = %q, want %q", got, want)
	}
}

func TestStartBuildsAuthorizationURL(t *testing.T) {
	f := &OpenRouterFlow{Rand: &fixedRand{}, Now: func() time.Time { return time.Unix(1700000000, 0) }}
	started, err := f.Start(OpenRouterStartOptions{
		CallbackURL: "http://127.0.0.1:20128/admin/provider/login/callback",
		KeyLabel:    "Codvian Router",
		Limit:       "12.50",
		Expiration:  "30-days",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	u, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatalf("authorization URL does not parse: %v", err)
	}
	if u.Scheme != "https" || u.Host != "openrouter.ai" || u.Path != "/auth" {
		t.Errorf("authorization URL base = %s://%s%s, want https://openrouter.ai/auth", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	verifier := fixedB64(0, 32)
	state := fixedB64(32, 32)
	if q.Get("code_challenge") != CodeChallenge(verifier) {
		t.Errorf("code_challenge = %q, want S256 of the generated verifier", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("state") != state || started.State != state {
		t.Errorf("state = %q / %q, want %q", q.Get("state"), started.State, state)
	}
	if q.Get("callback_url") != "http://127.0.0.1:20128/admin/provider/login/callback" {
		t.Errorf("callback_url = %q", q.Get("callback_url"))
	}
	if q.Get("key_label") != "Codvian Router" || q.Get("limit") != "12.50" || q.Get("expiration") != "30-days" {
		t.Errorf("optional params = %v", q)
	}
	if started.Headless {
		t.Error("callback flow reported headless")
	}
	if !started.ExpiresAt.Equal(time.Unix(1700000000, 0).Add(OpenRouterPendingTTL)) {
		t.Errorf("ExpiresAt = %v", started.ExpiresAt)
	}
	// The verifier must NEVER appear in the authorization URL.
	if strings.Contains(started.AuthorizationURL, verifier) {
		t.Fatal("authorization URL leaks the code_verifier")
	}
}

func TestStartHeadlessOmitsCallback(t *testing.T) {
	f := &OpenRouterFlow{Rand: &fixedRand{}}
	started, err := f.Start(OpenRouterStartOptions{KeyLabel: "Codvian Router"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !started.Headless {
		t.Error("no-callback flow not reported headless")
	}
	u, _ := url.Parse(started.AuthorizationURL)
	if u.Query().Has("callback_url") {
		t.Errorf("headless URL carries callback_url: %s", started.AuthorizationURL)
	}
	if u.Query().Get("code_challenge") == "" || u.Query().Get("code_challenge_method") != "S256" {
		t.Error("headless URL missing PKCE challenge")
	}
}

func TestStartValidation(t *testing.T) {
	cases := []struct {
		name string
		opts OpenRouterStartOptions
	}{
		{"http non-loopback callback", OpenRouterStartOptions{CallbackURL: "http://example.com/cb"}},
		{"ftp callback", OpenRouterStartOptions{CallbackURL: "ftp://127.0.0.1/cb"}},
		{"relative callback", OpenRouterStartOptions{CallbackURL: "/cb"}},
		{"zero limit", OpenRouterStartOptions{Limit: "0"}},
		{"negative limit", OpenRouterStartOptions{Limit: "-5"}},
		{"million limit", OpenRouterStartOptions{Limit: "1000000"}},
		{"three decimals", OpenRouterStartOptions{Limit: "1.234"}},
		{"non-numeric limit", OpenRouterStartOptions{Limit: "abc"}},
		{"bad expiration", OpenRouterStartOptions{Expiration: "2-days"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &OpenRouterFlow{Rand: &fixedRand{}}
			if _, err := f.Start(tc.opts); err == nil {
				t.Fatalf("Start(%+v) accepted an invalid option", tc.opts)
			}
		})
	}
	// Valid shapes are accepted.
	for _, ok := range []OpenRouterStartOptions{
		{CallbackURL: "https://app.example.com/cb"},
		{CallbackURL: "http://localhost:51423/callback"},
		{CallbackURL: "http://[::1]:8080/cb"},
		{Limit: "25"}, {Limit: "999999.99"}, {Expiration: "never"},
	} {
		f := &OpenRouterFlow{Rand: &fixedRand{}}
		if _, err := f.Start(ok); err != nil {
			t.Errorf("Start(%+v) rejected a valid option: %v", ok, err)
		}
	}
}

// exchangeMock is a local stand-in for OpenRouter's key-exchange
// endpoint: it records the request and answers with a fixed key.
type exchangeMock struct {
	srv      *httptest.Server
	lastBody openRouterExchangeRequest
	lastPath string
	status   int
	body     string
	calls    int
}

func newExchangeMock(t *testing.T) *exchangeMock {
	t.Helper()
	m := &exchangeMock{status: 200, body: `{"key":"sk-or-v1-mockkey"}`}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls++
		m.lastPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &m.lastBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(m.status)
		_, _ = w.Write([]byte(m.body))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func TestExchangeHappyPath(t *testing.T) {
	m := newExchangeMock(t)
	f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
	started, err := f.Start(OpenRouterStartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	key, err := f.Exchange(context.Background(), started.State, "auth-code-123")
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if key != "sk-or-v1-mockkey" {
		t.Errorf("key = %q, want the exchanged key", key)
	}
	if m.lastPath != "/api/v1/auth/keys" {
		t.Errorf("exchange path = %q, want /api/v1/auth/keys", m.lastPath)
	}
	if m.lastBody.Code != "auth-code-123" || m.lastBody.CodeChallengeMethod != "S256" {
		t.Errorf("exchange body = %+v", m.lastBody)
	}
	if m.lastBody.CodeVerifier != fixedB64(0, 32) {
		t.Errorf("exchange sent the wrong verifier")
	}
}

func TestExchangeStateSingleUse(t *testing.T) {
	m := newExchangeMock(t)
	f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
	started, _ := f.Start(OpenRouterStartOptions{})
	if _, err := f.Exchange(context.Background(), started.State, "code"); err != nil {
		t.Fatalf("first Exchange: %v", err)
	}
	if _, err := f.Exchange(context.Background(), started.State, "code"); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("second Exchange err = %v, want ErrUnknownState", err)
	}
	if m.calls != 1 {
		t.Errorf("exchange endpoint calls = %d, want 1 (state consumed once)", m.calls)
	}
}

func TestExchangeUnknownState(t *testing.T) {
	f := &OpenRouterFlow{Rand: &fixedRand{}}
	if _, err := f.Exchange(context.Background(), "never-issued", "code"); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("err = %v, want ErrUnknownState", err)
	}
	if _, err := f.Exchange(context.Background(), "", "code"); err == nil {
		t.Fatal("empty state accepted")
	}
	if _, err := f.Exchange(context.Background(), "s", ""); err == nil {
		t.Fatal("empty code accepted")
	}
}

func TestExchangeExpired(t *testing.T) {
	m := newExchangeMock(t)
	now := time.Unix(1700000000, 0)
	f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}, Now: func() time.Time { return now }}
	started, _ := f.Start(OpenRouterStartOptions{})
	now = now.Add(OpenRouterPendingTTL + time.Second)
	if _, err := f.Exchange(context.Background(), started.State, "code"); !errors.Is(err, ErrLoginExpired) {
		t.Fatalf("err = %v, want ErrLoginExpired", err)
	}
	if m.calls != 0 {
		t.Errorf("expired flow still hit the exchange endpoint (%d calls)", m.calls)
	}
}

func TestExchangeErrors(t *testing.T) {
	t.Run("403", func(t *testing.T) {
		m := newExchangeMock(t)
		m.status, m.body = 403, `{"error":"invalid code"}`
		f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
		started, _ := f.Start(OpenRouterStartOptions{})
		_, err := f.Exchange(context.Background(), started.State, "code")
		var ee *ExchangeError
		if !errors.As(err, &ee) || ee.Status != 403 {
			t.Fatalf("err = %v, want ExchangeError 403", err)
		}
		if strings.Contains(err.Error(), "invalid code") {
			t.Error("exchange error echoes the upstream body")
		}
	})
	t.Run("500", func(t *testing.T) {
		m := newExchangeMock(t)
		m.status, m.body = 500, `oops`
		f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
		started, _ := f.Start(OpenRouterStartOptions{})
		_, err := f.Exchange(context.Background(), started.State, "code")
		var ee *ExchangeError
		if !errors.As(err, &ee) || ee.Status != 500 {
			t.Fatalf("err = %v, want ExchangeError 500", err)
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		m := newExchangeMock(t)
		m.body = `not json`
		f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
		started, _ := f.Start(OpenRouterStartOptions{})
		if _, err := f.Exchange(context.Background(), started.State, "code"); err == nil {
			t.Fatal("malformed exchange response accepted")
		}
	})
	t.Run("empty key", func(t *testing.T) {
		m := newExchangeMock(t)
		m.body = `{"key":""}`
		f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
		started, _ := f.Start(OpenRouterStartOptions{})
		if _, err := f.Exchange(context.Background(), started.State, "code"); err == nil {
			t.Fatal("empty key accepted")
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		m := newExchangeMock(t)
		f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
		started, _ := f.Start(OpenRouterStartOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := f.Exchange(ctx, started.State, "code"); err == nil {
			t.Fatal("cancelled context accepted")
		}
	})
}

// seqRand yields unique 8-byte little-endian counter blocks, so
// every Start gets a distinct verifier/state (fixedRand wraps after
// 256 bytes and would collide states across many flows).
type seqRand struct{ n uint64 }

func (r *seqRand) Read(p []byte) (int, error) {
	for i := 0; i < len(p); i += 8 {
		var b [8]byte
		for j := 0; j < 8; j++ {
			b[j] = byte(r.n >> (8 * j))
		}
		r.n++
		copy(p[i:], b[:])
	}
	return len(p), nil
}

func TestPendingCapAndPrune(t *testing.T) {
	now := time.Unix(1700000000, 0)
	f := &OpenRouterFlow{Rand: &seqRand{}, Now: func() time.Time { return now }}
	for i := 0; i < openRouterMaxPending; i++ {
		if _, err := f.Start(OpenRouterStartOptions{}); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}
	if _, err := f.Start(OpenRouterStartOptions{}); err == nil {
		t.Fatal("pending map grew past its cap")
	}
	now = now.Add(OpenRouterPendingTTL + time.Second)
	if _, err := f.Start(OpenRouterStartOptions{}); err != nil {
		t.Fatalf("Start after expiry should prune: %v", err)
	}
}

func TestSecretsNeverInErrors(t *testing.T) {
	m := newExchangeMock(t)
	m.status, m.body = 403, `{"error":"bad"}`
	f := &OpenRouterFlow{AuthBase: m.srv.URL, Rand: &fixedRand{}}
	started, _ := f.Start(OpenRouterStartOptions{})
	_, err := f.Exchange(context.Background(), started.State, "code")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), fixedB64(0, 32)) {
		t.Error("error leaks the code_verifier")
	}
	// A successful exchange's key is returned, never embedded in
	// any error path of the flow itself.
	m.status, m.body = 200, `{"key":"sk-or-v1-secret"}`
	started2, _ := f.Start(OpenRouterStartOptions{})
	key, err := f.Exchange(context.Background(), started2.State, "code")
	if err != nil || key != "sk-or-v1-secret" {
		t.Fatalf("Exchange: %q %v", key, err)
	}
}

// TestStartCustomAuthBase proves the AuthBase override is honored
// (the mock-server tests rely on it; production uses the default).
func TestStartCustomAuthBase(t *testing.T) {
	f := &OpenRouterFlow{AuthBase: "https://mock.example/", Rand: &fixedRand{}}
	started, err := f.Start(OpenRouterStartOptions{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !strings.HasPrefix(started.AuthorizationURL, "https://mock.example/auth?") {
		t.Errorf("authorization URL = %q, want the custom base", started.AuthorizationURL)
	}
	if bytes.Contains([]byte(started.AuthorizationURL), []byte("//auth")) {
		t.Errorf("double slash in URL: %q", started.AuthorizationURL)
	}
}
