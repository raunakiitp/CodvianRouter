// Package main — B2 context-relay pin tests (PARITY row :489).
//
// V-B2-489's verdict: the three :489 producers (commit d3abe14) are
// real and in the request path, but NO producer is pinned by any
// committed test — the existing relay tests inject fake summarisers
// and a stubbed quota signal, so they pin the engine seam only. The
// summariser and the quota signal both live in closures inside
// openStack (cmd/codvian-router/main.go), unreachable from any test
// except through the real stack — so these tests build one (the
// T-B2-QUOTA-PIN pattern: openStack over httptest upstreams) and
// observe the producers through what the mocks receive:
//
//   - production quota signal (RelayQuotaUsedFor, main.go): the
//     success trigger fires only when the engine's learned quota
//     hint — measured from real upstream rate-limit headers on the
//     previous response — puts the account's used fraction in the
//     [0.85, 0.95) window. Test 1 teaches remaining 10/100 (used
//     0.90) and the want IS recorded; test 2 teaches remaining
//     50/100 (used 0.50) and nothing is recorded. With the signal
//     removed, test 1's second request gets no handoff and FAILS.
//   - production summariser (relaySummariser, main.go): the cheapest
//     priced model with a live client, called directly. The stack's
//     providers carry operator prices (codex $1/$2 per 1M, other
//     $10/$20), so the summariser must dial codex's upstream — and
//     the mock proves it: exactly one call, model "m", max_tokens
//     512, the structured summariser prompt, and the source messages
//     in "[n] ROLE:" form. The parsed summary is then injected as
//     the <context_handoff> of the next attempt. With the
//     summariser disabled, the handoff degrades to the raw
//     transcript and test 1 FAILS on the summary assertions.
//   - cross-request session store, through the real stack: the want
//     earned by request 1 under one gateway key is preloaded by
//     request 2 under the same key (test 1) and invisible to a
//     request under another key (test 3).
package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// relayPinMessage is one chat message of a recorded mock call.
type relayPinMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// relayPinCall is one decoded chat-completions request body received
// by a mock upstream.
type relayPinCall struct {
	Model     string            `json:"model"`
	Messages  []relayPinMessage `json:"messages"`
	MaxTokens int               `json:"max_tokens"`
}

// relayPinUpstream is a fake OpenAI-compatible upstream that records
// every chat call, split into summariser calls (the system prompt
// carries the summariser instruction) and relay calls (everything
// else). Relay calls answer 200 with the configured reply and,
// depending on rateLimitMode, rate-limit headers teaching a measured
// remaining fraction: mode 1 = 10 of 100 remain (used 0.90, inside
// the relay window), mode 2 = 50 of 100 remain (used 0.50, below
// it). Summariser calls answer with the structured summary and
// never carry rate-limit headers, so they never re-teach the hint.
type relayPinUpstream struct {
	reply string

	mu              sync.Mutex
	summariserCalls []relayPinCall
	relayCalls      []relayPinCall
	rateLimitMode   atomic.Int32 // 0 = none, 1 = 10/100, 2 = 50/100
}

func (u *relayPinUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body relayPinCall
	_ = json.Unmarshal(raw, &body)
	summariser := false
	for _, m := range body.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "concise conversation summariser") {
			summariser = true
			break
		}
	}
	u.mu.Lock()
	if summariser {
		u.summariserCalls = append(u.summariserCalls, body)
	} else {
		u.relayCalls = append(u.relayCalls, body)
	}
	mode := u.rateLimitMode.Load()
	u.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if !summariser {
		switch mode {
		case 1:
			w.Header().Set("x-ratelimit-limit-requests", "100")
			w.Header().Set("x-ratelimit-remaining-requests", "10")
			w.Header().Set("x-ratelimit-reset-requests", "15m")
		case 2:
			w.Header().Set("x-ratelimit-limit-requests", "100")
			w.Header().Set("x-ratelimit-remaining-requests", "50")
			w.Header().Set("x-ratelimit-reset-requests", "15m")
		}
	}
	content := u.reply
	if summariser {
		content = "SUMMARY: user asks what 2+2 is\nPROGRESS: question asked, no answer yet\nDECISIONS: - use arithmetic\nENTITIES: 2+2"
	}
	out, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "m",
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (u *relayPinUpstream) summariserCallCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.summariserCalls)
}

func (u *relayPinUpstream) summariserCall(i int) relayPinCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.summariserCalls[i]
}

func (u *relayPinUpstream) lastRelayCall() (relayPinCall, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.relayCalls) == 0 {
		return relayPinCall{}, false
	}
	return u.relayCalls[len(u.relayCalls)-1], true
}

// newRelayPinStack builds a real stack (openStack) over two no-auth
// providers — codex and other, both serving model "m" from httptest
// upstreams — in a throwaway temp data dir. Operator prices make
// codex the cheapest priced model ($1/$2 per 1M) and other ten times
// dearer ($10/$20), so the production summariser must pick codex.
// The mock upstreams are returned for mode flips and call inspection.
func newRelayPinStack(t *testing.T) (*stack, *relayPinUpstream, *relayPinUpstream) {
	t.Helper()
	codex := &relayPinUpstream{reply: "from-codex"}
	other := &relayPinUpstream{reply: "from-other"}
	codexSrv := httptest.NewServer(codex)
	otherSrv := httptest.NewServer(other)
	t.Cleanup(func() { codexSrv.Close(); otherSrv.Close() })

	dir := t.TempDir()
	cfgJSON, err := json.Marshal(map[string]any{
		"version":  1,
		"listen":   "127.0.0.1:20128",
		"data_dir": filepath.Join(dir, "data"),
		"providers": []map[string]any{
			{"id": "codex", "name": "codex", "base_url": codexSrv.URL, "wire_format": "openai", "enabled": true, "quota_hint": -1, "models": []map[string]any{{"id": "m"}}},
			{"id": "other", "name": "other", "base_url": otherSrv.URL, "wire_format": "openai", "enabled": true, "quota_hint": -1, "models": []map[string]any{{"id": "m"}}},
		},
		"provider_prices": map[string]any{
			"codex": map[string]any{"m": map[string]any{"input_usd_per_1m": 1.0, "output_usd_per_1m": 2.0}},
			"other": map[string]any{"m": map[string]any{"input_usd_per_1m": 10.0, "output_usd_per_1m": 20.0}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, cfgJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := openStack(options{configPath: cfgPath})
	if err != nil {
		t.Fatalf("openStack: %v", err)
	}
	// Shut the stack's stores down at test end, exactly like the
	// production shutdown path (runServe: syncCachesOnShutdown,
	// then asyncLed.Close() to flush, feedback.Close(),
	// led.Close()): the SQLite handles otherwise stay open and
	// Windows refuses TempDir removal ("file is being used by
	// another process"), failing the test at cleanup even though
	// every assertion passed.
	t.Cleanup(func() {
		st.syncCachesOnShutdown(context.Background())
		if st.asyncLed != nil {
			st.asyncLed.Close()
		}
		if st.feedback != nil {
			_ = st.feedback.Close()
		}
		if st.led != nil {
			_ = st.led.Close()
		}
	})
	return st, codex, other
}

// relayPinDrive runs one context-relay chat request down the given
// single-provider chain under the given gateway key and returns the
// served content.
func relayPinDrive(t *testing.T, st *stack, providerID, callerID string) string {
	t.Helper()
	req := provider.ChatRequest{
		Model:    "m",
		CallerID: callerID,
		Messages: []provider.Message{
			{Role: "system", Content: "be helpful"},
			{Role: "user", Content: "what is 2+2?"},
		},
	}
	resp, _, err := st.engine.ChatWithChainStrategy(context.Background(), []router.Attempt{{ProviderID: providerID, Model: "m"}}, req, "context-relay")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(%s, %s): %v", providerID, callerID, err)
	}
	return resp.Content
}

// TestB2RelayPinProductionSummariserAndQuotaSignal pins the two
// package-main producers at once: the production quota signal
// (RelayQuotaUsedFor from the learned hint) and the production
// summariser (cheapest priced model, called directly).
//
// Request 1: codex succeeds with rate-limit headers teaching 10 of
// 100 remaining — the engine learns hint 10, the signal reports
// used 0.90 (in the [0.85, 0.95) window), the want is recorded and
// stored under the gateway key. No summariser call happens yet: a
// terminal success never builds a handoff.
//
// Request 2 (same key, other serves): the preloaded want is built
// by the production summariser — codex's upstream must show exactly
// one direct call with the structured prompt, model "m" and
// max_tokens 512 — and the parsed summary is injected as the
// <context_handoff> of other's request.
func TestB2RelayPinProductionSummariserAndQuotaSignal(t *testing.T) {
	st, codex, other := newRelayPinStack(t)
	codex.rateLimitMode.Store(1) // remaining 10/100 -> used 0.90

	if got := relayPinDrive(t, st, "codex", "relay-pin-key-1"); got != "from-codex" {
		t.Fatalf("request 1 content = %q, want from-codex", got)
	}
	hint, expiresAt, ok := st.engine.LearnedQuotaHint("codex")
	if !ok || hint != 10 || expiresAt.IsZero() || time.Until(expiresAt) <= 0 {
		t.Fatalf("LearnedQuotaHint(codex) = (%d, %v, %v), want (10, future, true)", hint, expiresAt, ok)
	}
	if n := codex.summariserCallCount(); n != 0 {
		t.Fatalf("summariser calls after request 1 = %d, want 0 (lazy build: no next attempt)", n)
	}

	if got := relayPinDrive(t, st, "other", "relay-pin-key-1"); got != "from-other" {
		t.Fatalf("request 2 content = %q, want from-other", got)
	}
	if n := codex.summariserCallCount(); n != 1 {
		t.Fatalf("summariser calls = %d, want 1 (production summariser dialled the cheapest priced model once, directly)", n)
	}
	call := codex.summariserCall(0)
	if call.Model != "m" {
		t.Fatalf("summariser call model = %q, want m", call.Model)
	}
	if call.MaxTokens != 512 {
		t.Fatalf("summariser call max_tokens = %d, want 512", call.MaxTokens)
	}
	var sys, usr string
	for _, m := range call.Messages {
		if m.Role == "system" {
			sys = m.Content
		}
		if m.Role == "user" {
			usr = m.Content
		}
	}
	if !strings.Contains(sys, "You are a concise conversation summariser") {
		t.Fatalf("summariser call system prompt = %q, want the structured summariser instruction", sys)
	}
	for _, want := range []string{"[1] SYSTEM:", "be helpful", "[2] USER:", "what is 2+2?"} {
		if !strings.Contains(usr, want) {
			t.Fatalf("summariser call user content lacks %q: %q", want, usr)
		}
	}
	last, ok := other.lastRelayCall()
	if !ok {
		t.Fatal("other client received no relay call on request 2")
	}
	if len(last.Messages) == 0 || last.Messages[0].Role != "system" || !strings.Contains(last.Messages[0].Content, "<context_handoff>") {
		t.Fatalf("request 2 got no injected handoff; messages = %+v", last.Messages)
	}
	handoff := last.Messages[0].Content
	for _, want := range []string{"user asks what 2+2 is", "question asked, no answer yet", "use arithmetic", "2+2"} {
		if !strings.Contains(handoff, want) {
			t.Fatalf("injected handoff lacks %q: %q", want, handoff)
		}
	}
	if strings.Contains(handoff, "Unsummarised transcript") {
		t.Fatalf("summarised handoff carries the unsummarised marker: %q", handoff)
	}
	rst, ok := st.engine.LastRelayStatus()
	if !ok || !rst.HandoffActive || !rst.HandoffSummarised || rst.DegradeReason != "" {
		t.Fatalf("relay status ok=%v active=%v summarised=%v reason=%q, want true/true/true/\"\"", ok, rst.HandoffActive, rst.HandoffSummarised, rst.DegradeReason)
	}
}

// TestB2RelayPinQuotaSignalWindow pins the trigger gate: a success
// whose learned used fraction is BELOW the 0.85 threshold records
// no want — the next request under the same key starts cold, and
// the summariser is never called.
func TestB2RelayPinQuotaSignalWindow(t *testing.T) {
	st, codex, other := newRelayPinStack(t)
	codex.rateLimitMode.Store(2) // remaining 50/100 -> used 0.50

	if got := relayPinDrive(t, st, "codex", "relay-pin-key-2"); got != "from-codex" {
		t.Fatalf("request 1 content = %q, want from-codex", got)
	}
	hint, _, ok := st.engine.LearnedQuotaHint("codex")
	if !ok || hint != 50 {
		t.Fatalf("LearnedQuotaHint(codex) = (%d, %v), want (50, true)", hint, ok)
	}

	if got := relayPinDrive(t, st, "other", "relay-pin-key-2"); got != "from-other" {
		t.Fatalf("request 2 content = %q, want from-other", got)
	}
	if n := codex.summariserCallCount(); n != 0 {
		t.Fatalf("summariser calls = %d, want 0 (used 0.50 is below the relay window, no want recorded)", n)
	}
	last, ok := other.lastRelayCall()
	if !ok {
		t.Fatal("other client received no relay call on request 2")
	}
	if len(last.Messages) != 2 || last.Messages[0].Content != "be helpful" {
		t.Fatalf("request 2 messages = %+v, want the 2 originals with no handoff", last.Messages)
	}
	rst, ok := st.engine.LastRelayStatus()
	if !ok || rst.HandoffActive {
		t.Fatalf("relay status ok=%v active=%v, want true/false", ok, rst.HandoffActive)
	}
}

// TestB2RelayPinWantKeyedByGatewayKey pins the store's keying
// through the real stack: a want earned under one gateway key is
// invisible to a request under another key, and still waiting for
// the original key afterwards.
func TestB2RelayPinWantKeyedByGatewayKey(t *testing.T) {
	st, codex, other := newRelayPinStack(t)
	codex.rateLimitMode.Store(1) // remaining 10/100 -> used 0.90

	if got := relayPinDrive(t, st, "codex", "relay-pin-key-A"); got != "from-codex" {
		t.Fatalf("request 1 (key-A) content = %q, want from-codex", got)
	}
	if got := relayPinDrive(t, st, "other", "relay-pin-key-B"); got != "from-other" {
		t.Fatalf("request 2 (key-B) content = %q, want from-other", got)
	}
	if n := codex.summariserCallCount(); n != 0 {
		t.Fatalf("summariser calls after key-B request = %d, want 0 (no want stored under key-B)", n)
	}
	last, _ := other.lastRelayCall()
	if len(last.Messages) != 2 {
		t.Fatalf("request 2 (key-B) messages = %+v, want the 2 originals with no handoff", last.Messages)
	}

	if got := relayPinDrive(t, st, "other", "relay-pin-key-A"); got != "from-other" {
		t.Fatalf("request 3 (key-A) content = %q, want from-other", got)
	}
	if n := codex.summariserCallCount(); n != 1 {
		t.Fatalf("summariser calls = %d, want 1 (key-A's want still waiting)", n)
	}
	last, _ = other.lastRelayCall()
	if len(last.Messages) == 0 || !strings.Contains(last.Messages[0].Content, "<context_handoff>") {
		t.Fatalf("request 3 (key-A) got no injected handoff; messages = %+v", last.Messages)
	}
	if !strings.Contains(last.Messages[0].Content, "user asks what 2+2 is") {
		t.Fatalf("request 3 (key-A) handoff lacks the parsed summary: %q", last.Messages[0].Content)
	}
}
