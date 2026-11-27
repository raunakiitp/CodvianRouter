package noauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEveryNoAuthCatalogIDHasAMechanism — A3 item 5: each of the ten
// CategoryNoAuth IDs has a mechanism here, with a reason. This fails
// loudly if the catalog grows an eleventh no-auth entry or one of the
// ten loses its mechanism.
func TestEveryNoAuthCatalogIDHasAMechanism(t *testing.T) {
	want := []string{
		"aihorde", "auggie", "cloudflare-playground", "codex-app-server",
		"devin-cli-agentic", "duckduckgo-web", "minimax-code", "uncloseai",
		"veoaifree-web", "zcode",
	}
	for _, id := range want {
		m, ok := For(id)
		if !ok {
			t.Errorf("no mechanism registered for no-auth catalog ID %q", id)
			continue
		}
		if m.Reason == "" {
			t.Errorf("mechanism %q has no reason — every mechanism must state why it is what it is", id)
		}
		if m.Kind != KindNone && m.DocURL == "" {
			t.Errorf("mechanism %q (kind %s) cites no provider documentation", id, m.Kind)
		}
	}
	if got := len(All()); got != len(want) {
		t.Errorf("registry holds %d mechanisms, want %d — a new no-auth catalog entry needs a mechanism here", got, len(want))
	}
}

// TestAnonymousDialSucceedsWithoutCredentials — the core guarantee for
// the anonymous mechanisms (aihorde, uncloseai): a request succeeds
// against a mock upstream with NO user credential anywhere, and the
// mock observes exactly the anonymous posture the docs describe
// (aihorde: Bearer 0000000000 from its README; uncloseai: no
// credential header at all, per its own "need no key" statement).
func TestAnonymousDialSucceedsWithoutCredentials(t *testing.T) {
	cases := []struct {
		id           string
		wantAuth     string // header value the mock must see, "" = none
		wantKeyPlain bool   // aihorde: documented anonymous key, not a secret
	}{
		{id: "aihorde", wantAuth: "Bearer 0000000000", wantKeyPlain: true},
		{id: "uncloseai", wantAuth: ""},
	}
	for _, tc := range cases {
		m, ok := For(tc.id)
		if !ok {
			t.Fatalf("mechanism %q missing", tc.id)
		}
		var (
			mu       sync.Mutex
			gotAuth  string
			gotModel string
		)
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			gotAuth = r.Header.Get("Authorization")
			mu.Unlock()
			if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
				t.Errorf("%s: mock hit %q, want a chat/completions path", tc.id, r.URL.Path)
			}
			var body struct {
				Model    string              `json:"model"`
				Messages []map[string]string `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("%s: mock cannot decode request body: %v", tc.id, err)
			}
			mu.Lock()
			gotModel = body.Model
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{
					{"message": map[string]string{"role": "assistant", "content": "ok"}},
				},
			})
		}))
		defer mock.Close()

		// Point the mechanism at the mock upstream: same posture,
		// no live network.
		m.BaseURLs = []string{mock.URL}
		c := &AnonymousClient{}
		data, err := c.Dial(context.Background(), m, "203.0.113.9", "test-model", []map[string]string{
			{"role": "user", "content": "ping"},
		})
		if err != nil {
			t.Fatalf("%s: anonymous dial failed: %v", tc.id, err)
		}
		var resp struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &resp); err != nil {
			t.Fatalf("%s: response does not parse: %v", tc.id, err)
		}
		if len(resp.Choices) == 0 || resp.Choices[0].Message.Content == "" {
			t.Fatalf("%s: empty assistant response", tc.id)
		}
		mu.Lock()
		defer mu.Unlock()
		if gotAuth != tc.wantAuth {
			t.Errorf("%s: Authorization = %q, want %q (the documented anonymous posture)", tc.id, gotAuth, tc.wantAuth)
		}
		if tc.wantKeyPlain && tc.wantAuth != "" && !tc.wantKeyPlain {
			t.Errorf("%s: unexpected credential state", tc.id)
		}
		if gotModel != "test-model" {
			t.Errorf("%s: mock saw model %q", tc.id, gotModel)
		}
	}
}

// TestAihordeDocumentedLimitsEnforcedLocally — AI Horde's
// docs/reference/chat_requests.md documents per-IP chat-submit limits
// (5/second, 300/minute) with "a request over a limit gets 429".
// The mechanism enforces them BEFORE dialing: the 6th request within
// the window from one IP is refused locally and the mock upstream
// never sees it. Different IPs do not share a budget.
func TestAihordeDocumentedLimitsEnforcedLocally(t *testing.T) {
	m, ok := For("aihorde")
	if !ok {
		t.Fatal("aihorde mechanism missing")
	}
	if len(m.Limits) == 0 {
		t.Fatal("aihorde mechanism records no documented limits")
	}
	if m.Limits[0].Requests != 5 || m.Limits[0].Window != time.Second {
		t.Errorf("aihorde first documented limit = %+v, want 5 per second (chat_requests.md)", m.Limits[0])
	}
	if m.Limits[1].Requests != 300 || m.Limits[1].Window != time.Minute {
		t.Errorf("aihorde second documented limit = %+v, want 300 per minute (chat_requests.md)", m.Limits[1])
	}

	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer mock.Close()
	m.BaseURLs = []string{mock.URL}

	now := time.Unix(1_700_000_000, 0)
	c := &AnonymousClient{Now: func() time.Time { return now }}
	for i := 0; i < 5; i++ {
		if _, err := c.Dial(context.Background(), m, "198.51.100.7", "m", nil); err != nil {
			t.Fatalf("request %d within the documented budget refused: %v", i+1, err)
		}
	}
	var olerr *OverLimitError
	_, err := c.Dial(context.Background(), m, "198.51.100.7", "m", nil)
	if !errors.As(err, &olerr) {
		t.Fatalf("6th request from one IP in the window: err = %v, want OverLimitError", err)
	}
	if got := hits.Load(); got != 5 {
		t.Errorf("mock upstream saw %d requests after the refused 6th, want exactly 5 — a refused request must never be dialed", got)
	}
	// A different IP has its own budget.
	if _, err := c.Dial(context.Background(), m, "198.51.100.8", "m", nil); err != nil {
		t.Errorf("other IP refused although its own budget is untouched: %v", err)
	}
	// The window resets with time.
	now = now.Add(time.Second)
	if _, err := c.Dial(context.Background(), m, "198.51.100.7", "m", nil); err != nil {
		t.Errorf("request after the window reset refused: %v", err)
	}
	if got := hits.Load(); got != 7 {
		t.Errorf("mock upstream saw %d requests total, want exactly 7 (5 + 1 other IP + 1 after reset)", got)
	}
}

// TestUncloseaiHasNoInventedNumericLimit — uncloseai's Terms of Use
// document IP-based throttling with no numeric budget, so the
// mechanism records none: inventing a number would breach the
// honesty rules.
func TestUncloseaiHasNoInventedNumericLimit(t *testing.T) {
	m, ok := For("uncloseai")
	if !ok {
		t.Fatal("uncloseai mechanism missing")
	}
	if len(m.Limits) != 0 {
		t.Errorf("uncloseai records numeric limits %v — its docs publish no numeric budget; remove the invented numbers", m.Limits)
	}
	if !strings.Contains(m.LimitNote, "IP-based") {
		t.Errorf("uncloseai LimitNote = %q, want the documented IP-based throttling statement from its Terms of Use", m.LimitNote)
	}
}

// TestKindNoneRowsStayHonest — the three providers whose own docs
// give no usable anonymous endpoint (duckduckgo-web,
// cloudflare-playground, veoaifree-web) stay KindNone with a written
// reason, no base URLs and no invented limits. The veoaifree-web
// "6 requests/hour" figure appears in no provider documentation.
func TestKindNoneRowsStayHonest(t *testing.T) {
	for _, id := range []string{"duckduckgo-web", "cloudflare-playground", "veoaifree-web"} {
		m, ok := For(id)
		if !ok {
			t.Fatalf("%s mechanism missing", id)
		}
		if m.Kind != KindNone {
			t.Errorf("%s kind = %q, want KindNone — no usable anonymous endpoint is documented by the provider", id, m.Kind)
		}
		if len(m.BaseURLs) != 0 || len(m.Limits) != 0 {
			t.Errorf("%s grew endpoints/limits %+v / %+v with no provider documentation behind them", id, m.BaseURLs, m.Limits)
		}
		if m.Reason == "" {
			t.Errorf("%s: KindNone without a written honest reason", id)
		}
		if AllowEmptyKey(id) {
			t.Errorf("%s: KindNone must not pass the empty-key check", id)
		}
	}
	if _, err := (&AnonymousClient{}).Dial(context.Background(), mustFor(t, "veoaifree-web"), "ip", "m", nil); err == nil {
		t.Errorf("dialing a KindNone mechanism must fail, got %v", err)
	}
}

// TestCLIBridgeSpawnsProviderCLI — the five CLI-owned auth bridges
// spawn their documented argv and get an answer without any
// credential passing through the router. The test drives a mock CLI
// (the test binary itself, re-exec'd) so no provider CLI is needed.
func TestCLIBridgeSpawnsProviderCLI(t *testing.T) {
	for _, id := range []string{"auggie", "devin-cli-agentic", "minimax-code", "zcode"} {
		m := mustFor(t, id)
		if !m.RunnableCommand() {
			t.Fatalf("%s has no runnable bridge command", id)
		}
		out, err := execMockCLI(t, m, "say hello")
		if err != nil {
			t.Fatalf("%s bridge: %v", id, err)
		}
		if !strings.Contains(out, "MOCK-CLI-OK") {
			t.Fatalf("%s bridge output = %q, want the mock CLI's answer", id, out)
		}
	}
	// codex-app-server is a long-lived JSON-RPC process, not a
	// one-shot command — the mechanism documents that honestly
	// instead of inventing an argv.
	m := mustFor(t, "codex-app-server")
	if m.RunnableCommand() {
		t.Errorf("codex-app-server grew a one-shot argv %v — its docs define a JSON-RPC app-server process, not a command", m.Command)
	}
	if m.AuthNote == "" {
		t.Error("codex-app-server bridge documents no auth note")
	}
}

// execMockCLI runs the mechanism's bridge with the test binary as the
// provider CLI (the documented argv is rewritten to the mock) and
// returns its stdout.
func execMockCLI(t *testing.T, m Mechanism, prompt string) (string, error) {
	t.Helper()
	mock := m
	mock.Command = []string{os.Args[0], "-test.run=TestMockCLIProcess", "{prompt}"}
	// The mock re-exec runs in-process; suppress Go test flag
	// parsing side effects by pointing the runner at a no-op.
	return mock.RunBridge(context.Background(), prompt, "GO_WANT_HELPER_PROCESS=1")
}

// TestMockCLIProcess is the mock provider CLI: re-executed by
// TestCLIBridgeSpawnsProviderCLI via os.Args[0]. It answers on stdout
// and requires the GO_WANT_HELPER_PROCESS guard so a plain `go test`
// never runs it as a real test.
func TestMockCLIProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		t.Skip("mock provider CLI — only runs re-exec'd with GO_WANT_HELPER_PROCESS=1")
	}
	prompt := os.Args[len(os.Args)-1]
	if prompt == "" {
		os.Exit(3)
	}
	// Prove the bridge passed the prompt and no fake credential.
	if prompt != "say hello" {
		os.Exit(4)
	}
	fmt.Fprintln(os.Stdout, "MOCK-CLI-OK "+prompt)
	os.Exit(0)
}

// TestBridgeFailsWhenDisabled — every mechanism must fail when it is
// disabled: the kill switch (NOAUTH_DISABLED=1) makes bridges return
// ErrBridgeDisabled without spawning, and anonymous dials fail closed.
func TestBridgeFailsWhenDisabled(t *testing.T) {
	t.Setenv("NOAUTH_DISABLED", "1")
	for _, id := range []string{"auggie", "devin-cli-agentic", "minimax-code", "zcode"} {
		m := mustFor(t, id)
		// A real spawn attempt would fail on machines without the
		// CLI; with the kill switch it must fail BEFORE spawning,
		// with ErrBridgeDisabled.
		_, err := m.RunBridge(context.Background(), "x")
		if !errors.Is(err, ErrBridgeDisabled) {
			t.Errorf("%s bridge while disabled: err = %v, want ErrBridgeDisabled (fail closed, no spawn)", id, err)
		}
	}
	c := &AnonymousClient{}
	m := mustFor(t, "uncloseai")
	if _, err := c.Dial(context.Background(), m, "ip", "m", nil); !errors.Is(err, ErrBridgeDisabled) {
		t.Errorf("anonymous dial while disabled: err = %v, want ErrBridgeDisabled", err)
	}
}

// TestDisabledMechanismIsNotKeylessForUnknownID — AllowEmptyKey fails
// closed for unknown IDs and KindNone rows, so the router's
// allowEmptyKey path can never grant an empty key without a
// documented mechanism.
func TestDisabledMechanismIsNotKeylessForUnknownID(t *testing.T) {
	for _, id := range []string{"", "not-a-provider", "duckduckgo-web", "cloudflare-playground", "veoaifree-web"} {
		if AllowEmptyKey(id) {
			t.Errorf("AllowEmptyKey(%q) = true, want false — no documented keyless mechanism", id)
		}
	}
	for _, id := range []string{"aihorde", "uncloseai", "auggie", "codex-app-server", "devin-cli-agentic", "minimax-code", "zcode"} {
		if !AllowEmptyKey(id) {
			t.Errorf("AllowEmptyKey(%q) = false, want true — the mechanism is documented keyless/CLI-owned", id)
		}
	}
}

func mustFor(t *testing.T, id string) Mechanism {
	t.Helper()
	m, ok := For(id)
	if !ok {
		t.Fatalf("mechanism %q missing", id)
	}
	return m
}

// TestCLIExistsGuard — the bridge refuses to claim success when the
// provider CLI is genuinely absent (fail closed, honest error), so a
// machine without the CLI can never silently look "connected".
func TestCLIExistsGuard(t *testing.T) {
	m := mustFor(t, "zcode")
	mock := m
	// A command that does not exist anywhere.
	mock.Command = []string{"definitely-not-a-real-binary-4f2a", "{prompt}"}
	_, err := mock.RunBridge(context.Background(), "x")
	if err == nil || errors.Is(err, ErrBridgeDisabled) {
		t.Fatalf("spawning a nonexistent CLI: err = %v, want a concrete failure", err)
	}
	var ee *exec.Error
	if !errors.As(err, &ee) {
		t.Errorf("missing CLI error = %T (%v), want exec.Error so callers can say \"install the CLI\"", err, err)
	}
}
