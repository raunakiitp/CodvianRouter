package server

// Auto prompt_cache_key end to end (CHECKLIST 131-132; research-log
// entry 184): one POST /v1/chat/completions through the real Server
// + real router.Engine + real provider clients against local mocks.
// The "OpenAI" mock is reached through a transport that rewrites
// the api.openai.com host to the mock, so the client believes it is
// talking to OpenAI's own API while the bytes land on a recorder.
//
// The test proves, from evidence rather than construction: the
// upstream body carries a "codvian-" auto key; a different user turn
// with the same system prompt gets the SAME key; a third-party
// endpoint gets NO generated key; the client-facing response and
// the ledger carry the provider-reported cached tokens; and the
// auto key is absent from history — the logs feed contains no
// prompt_cache_key and no codvian- string anywhere.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// cacheKeyRewriteTransport rewrites a request's scheme+host to a
// local mock while keeping the original path: the client believes it
// is talking to api.openai.com, the bytes land on the recorder.
type cacheKeyRewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *cacheKeyRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.base.RoundTrip(clone)
}

// cacheKeyTestLedger adapts the SQLite ledger to the router's Ledger
// interface the same way production does (cmd/codvian-router's
// ledgerAdapter): the split is known only when the upstream reported
// at least one side of it, and the provider-reported cached tokens
// ride along with the split.
type cacheKeyTestLedger struct {
	l *ledger.Ledger
}

func (a *cacheKeyTestLedger) RecordAttempt(providerName, model string, ok bool, latencyMs int64, usage provider.Usage) {
	if a == nil || a.l == nil {
		return
	}
	_, _ = a.l.RecordUsage(context.Background(), ledger.Usage{
		GatewayKeyID:     "",
		Provider:         providerName,
		Model:            model,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		CachedTokens:     usage.CachedTokens,
		SplitKnown:       usage.PromptTokens != 0 || usage.CompletionTokens != 0,
		OK:               ok,
		LatencyMS:        latencyMs,
	}, nil)
}

func TestChatCompletionsAutoPromptCacheKeyEndToEnd(t *testing.T) {
	// "OpenAI" mock: records every upstream body, answers with
	// provider-reported cached tokens.
	var openAIBodies []map[string]json.RawMessage
	openAISrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		openAIBodies = append(openAIBodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-oai","object":"chat.completion","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"openai says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,"prompt_tokens_details":{"cached_tokens":800}}}`))
	}))
	defer openAISrv.Close()
	openAITarget, err := url.Parse(openAISrv.URL)
	if err != nil {
		t.Fatalf("parse mock URL: %v", err)
	}
	openAIClient, err := provider.NewClient("https://api.openai.com", "sk-openai",
		provider.WithHTTPClient(&http.Client{
			Transport: &cacheKeyRewriteTransport{target: openAITarget, base: http.DefaultTransport},
			Timeout:   5 * time.Second,
		}))
	if err != nil {
		t.Fatalf("NewClient openai: %v", err)
	}

	// Third-party OpenAI-compatible mock: same shape, different host.
	var otherBodies []map[string]json.RawMessage
	otherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		otherBodies = append(otherBodies, raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-other","object":"chat.completion","model":"other-model","choices":[{"message":{"role":"assistant","content":"other says hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":40}}}`))
	}))
	defer otherSrv.Close()
	otherClient, err := provider.NewClient(otherSrv.URL, "other-key", provider.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	if err != nil {
		t.Fatalf("NewClient other: %v", err)
	}

	led, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open: %v", err)
	}
	t.Cleanup(func() { led.Close() })

	eng := router.NewEngine(map[string]provider.Client{"openai": openAIClient, "other": otherClient},
		&cacheKeyTestLedger{l: led}, router.Config{
			MaxAttempts:      5,
			HedgeDelay:       -1, // hedging off: deterministic attempt order
			BackoffBase:      time.Millisecond,
			BackoffMax:       5 * time.Millisecond,
			BreakerThreshold: 5,
			BreakerOpenFor:   time.Minute,
			RetryAfterCap:    30 * time.Second,
			ChainFor: func(model string) []router.Attempt {
				switch model {
				case "gpt-4o":
					return []router.Attempt{{ProviderID: "openai", Model: "gpt-4o"}}
				case "other-model":
					return []router.Attempt{{ProviderID: "other", Model: "other-model"}}
				}
				return nil
			},
		})
	rec := metrics.NewRecorder()
	srv := newTestServer(t, eng, WithMetrics(rec), WithLedger(led), WithTimeout(10*time.Second))

	body := func(user string) string {
		return `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"` + user + `"}]}`
	}
	var out struct {
		Choices []struct {
			Message provider.Message `json:"message"`
		} `json:"choices"`
		Usage provider.Usage `json:"usage"`
	}
	resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body("Write a function."), "test-key-123")
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "openai says hi" {
		t.Fatalf("choices = %+v, want the mock's content", out.Choices)
	}
	if out.Usage.CachedTokens != 800 || out.Usage.PromptTokens != 1000 {
		t.Fatalf("response usage = %+v, want prompt 1000 cached 800", out.Usage)
	}
	if len(openAIBodies) != 1 {
		t.Fatalf("openai mock hit %d times, want 1", len(openAIBodies))
	}
	keyOf := func(raw map[string]json.RawMessage) (string, bool) {
		v, ok := raw["prompt_cache_key"]
		if !ok {
			return "", false
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", false
		}
		return s, true
	}
	key1, present := keyOf(openAIBodies[0])
	if !present || !strings.HasPrefix(key1, "codvian-") {
		t.Fatalf("auto key = %q (present %v), want a codvian- key", key1, present)
	}

	// A different user turn with the same system prompt gets the
	// SAME key — user messages are not part of it.
	resp2 := doReq(t, srv, http.MethodPost, "/v1/chat/completions", body("Something else entirely."), "test-key-123")
	if resp2.Code != http.StatusOK {
		t.Fatalf("second status = %d, want 200 (body %s)", resp2.Code, resp2.Body.String())
	}
	if len(openAIBodies) != 2 {
		t.Fatalf("openai mock hit %d times, want 2", len(openAIBodies))
	}
	key2, present := keyOf(openAIBodies[1])
	if !present || key2 != key1 {
		t.Fatalf("second attempt key = %q (present %v), want the same key %q", key2, present, key1)
	}

	// The third-party endpoint gets NO generated key, even with a
	// caller and a system prompt.
	resp3 := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"other-model","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"hi"}]}`, "test-key-123")
	if resp3.Code != http.StatusOK {
		t.Fatalf("other status = %d, want 200 (body %s)", resp3.Code, resp3.Body.String())
	}
	if len(otherBodies) != 1 {
		t.Fatalf("other mock hit %d times, want 1", len(otherBodies))
	}
	if _, present := keyOf(otherBodies[0]); present {
		t.Fatal("third-party endpoint received a generated prompt_cache_key")
	}

	// History must not contain the key: the logs feed carries the
	// provider-reported cached tokens, and no prompt_cache_key or
	// codvian- string anywhere in the payload.
	logs := doReq(t, srv, http.MethodGet, "/dashboard/logs?limit=10&filter=all", "", "")
	if logs.Code != http.StatusOK {
		t.Fatalf("logs status = %d, want 200", logs.Code)
	}
	if strings.Contains(logs.Body.String(), "prompt_cache_key") || strings.Contains(logs.Body.String(), "codvian-") {
		t.Fatalf("logs feed leaks the auto key: %s", logs.Body.String())
	}
	var logsOut struct {
		Events []ledger.UsageEvent `json:"events"`
	}
	if err := json.Unmarshal(logs.Body.Bytes(), &logsOut); err != nil {
		t.Fatalf("decode logs: %v", err)
	}
	if len(logsOut.Events) != 3 {
		t.Fatalf("logs events = %d, want 3", len(logsOut.Events))
	}
	// Newest first: other-model (cached 40), gpt-4o #2, gpt-4o #1.
	if logsOut.Events[0].CachedTokens != 40 || logsOut.Events[0].Model != "other-model" {
		t.Fatalf("newest log event = %+v, want other-model cached 40", logsOut.Events[0])
	}
	if logsOut.Events[1].CachedTokens != 800 || logsOut.Events[1].Model != "gpt-4o" {
		t.Fatalf("second log event = %+v, want gpt-4o cached 800", logsOut.Events[1])
	}
	if logsOut.Events[2].CachedTokens != 800 || logsOut.Events[2].PromptTokens != 1000 {
		t.Fatalf("third log event = %+v, want gpt-4o cached 800 prompt 1000", logsOut.Events[2])
	}

	// Overview cache aggregate: 2 of 3 requests reported cached
	// tokens, 1640 cached of 2100 prompt, top combo gpt-4o.
	ov := doReq(t, srv, http.MethodGet, "/dashboard/overview?range=24h", "", "")
	if ov.Code != http.StatusOK {
		t.Fatalf("overview status = %d, want 200", ov.Code)
	}
	if strings.Contains(ov.Body.String(), "prompt_cache_key") || strings.Contains(ov.Body.String(), "codvian-") {
		t.Fatalf("overview feed leaks the auto key: %s", ov.Body.String())
	}
	var ovOut struct {
		Requests int `json:"requests"`
		Cache    struct {
			RequestsWithCachedTokens int `json:"requests_with_cached_tokens"`
			CachedTokens             int `json:"cached_tokens"`
			PromptTokens             int `json:"prompt_tokens"`
			Combos                   []struct {
				Provider                 string `json:"provider"`
				Model                    string `json:"model"`
				Requests                 int    `json:"requests"`
				RequestsWithCachedTokens int    `json:"requests_with_cached_tokens"`
				CachedTokens             int    `json:"cached_tokens"`
				PromptTokens             int    `json:"prompt_tokens"`
			} `json:"combos"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(ov.Body.Bytes(), &ovOut); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if ovOut.Requests != 3 {
		t.Fatalf("overview requests = %d, want 3", ovOut.Requests)
	}
	if ovOut.Cache.RequestsWithCachedTokens != 3 || ovOut.Cache.CachedTokens != 1640 || ovOut.Cache.PromptTokens != 2100 {
		t.Fatalf("overview cache = %+v, want with-cached 3 cached 1640 prompt 2100", ovOut.Cache)
	}
	if len(ovOut.Cache.Combos) != 2 {
		t.Fatalf("overview combos = %d, want 2", len(ovOut.Cache.Combos))
	}
	top := ovOut.Cache.Combos[0]
	if top.Model != "gpt-4o" || top.Provider != "openai" || top.Requests != 2 ||
		top.RequestsWithCachedTokens != 2 || top.CachedTokens != 1600 || top.PromptTokens != 2000 {
		t.Fatalf("top combo = %+v, want gpt-4o/openai requests 2 with-cached 2 cached 1600 prompt 2000", top)
	}
	second := ovOut.Cache.Combos[1]
	if second.Model != "other-model" || second.CachedTokens != 40 || second.RequestsWithCachedTokens != 1 {
		t.Fatalf("second combo = %+v, want other-model cached 40 with-cached 1", second)
	}
}
