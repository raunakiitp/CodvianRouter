package server

// G1 (streaming usage accounting) server-level regression tests:
// a client that asks for stream_options.include_usage receives the
// upstream's reported usage as a final usage chunk (empty choices
// array) before [DONE]; without the request flag no usage chunk is
// emitted; when the upstream reported nothing, no usage chunk is
// invented. The upstream request always carries include_usage on
// the OpenAI wire (client-side test covers the flag itself).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// streamUsageUpstream serves a scripted SSE body and records the
// request body it saw.
func streamUsageUpstream(t *testing.T, sse string) (*httptest.Server, *[]byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func streamUsageServer(t *testing.T, upstreamURL string) *Server {
	t.Helper()
	client, err := provider.NewClient(upstreamURL, "upstream-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"p1": client}, nil, router.Config{
		MaxAttempts:      3,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "p1", Model: model}}
		},
	})
	return newTestServer(t, eng, WithTimeout(30*time.Second))
}

const streamUsageSSEWithReport = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":4}}}\n\n" +
	"data: [DONE]\n\n"

const streamUsageSSEWithoutReport = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gate-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

// dataLines returns the SSE data payloads (without the "data: "
// prefix), in order.
func dataLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimPrefix(line, "data: "))
		}
	}
	return out
}

func TestStreamIncludeUsageEmitsUsageChunk(t *testing.T) {
	up, upBody := streamUsageUpstream(t, streamUsageSSEWithReport)
	srv := streamUsageServer(t, up.URL)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The gateway asked the upstream for usage.
	var req map[string]any
	if err := json.Unmarshal(*upBody, &req); err != nil {
		t.Fatalf("upstream body not JSON: %v", err)
	}
	so, ok := req["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("upstream stream_options = %v, want include_usage:true", req["stream_options"])
	}
	lines := dataLines(rec.Body.String())
	if len(lines) < 2 || lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("stream does not end with [DONE]: %v", lines)
	}
	// The last data chunk before [DONE] is the usage chunk:
	// empty choices array + the upstream's reported numbers.
	var usageChunk struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			CachedTokens        int `json:"cached_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-2]), &usageChunk); err != nil {
		t.Fatalf("final chunk not JSON: %v (%s)", err, lines[len(lines)-2])
	}
	if usageChunk.Usage == nil {
		t.Fatalf("final chunk before [DONE] has no usage: %s", lines[len(lines)-2])
	}
	if len(usageChunk.Choices) != 0 {
		t.Errorf("usage chunk choices = %d entries, want empty array", len(usageChunk.Choices))
	}
	u := usageChunk.Usage
	if u.PromptTokens != 12 || u.CompletionTokens != 6 || u.TotalTokens != 18 || u.CachedTokens != 4 {
		t.Errorf("usage chunk = %+v, want the upstream report 12/6/18 cached 4", u)
	}
	// OpenAI SDK parity: the nested detail block rides alongside
	// the flat counts when the upstream reported cached tokens.
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 4 {
		t.Errorf("prompt_tokens_details = %+v, want cached_tokens 4", u.PromptTokensDetails)
	}
	// Exactly one usage chunk — no duplicate accounting on the wire.
	n := 0
	for _, l := range lines {
		if strings.Contains(l, `"usage"`) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("usage chunks = %d, want exactly 1", n)
	}
}

func TestStreamWithoutIncludeUsageOmitsUsageChunk(t *testing.T) {
	up, _ := streamUsageUpstream(t, streamUsageSSEWithReport)
	srv := streamUsageServer(t, up.URL)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	for _, l := range dataLines(rec.Body.String()) {
		if strings.Contains(l, `"usage"`) {
			t.Fatalf("usage chunk emitted without include_usage: %s", l)
		}
	}
}

func TestStreamIncludeUsageNoReportOmitsUsageChunk(t *testing.T) {
	up, _ := streamUsageUpstream(t, streamUsageSSEWithoutReport)
	srv := streamUsageServer(t, up.URL)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	// The upstream reported nothing: no usage chunk is invented
	// (a local estimate is never presented as provider-reported).
	for _, l := range dataLines(rec.Body.String()) {
		if strings.Contains(l, `"usage"`) {
			t.Fatalf("usage chunk invented without an upstream report: %s", l)
		}
	}
}

// TestStreamBudgetOverageRecorded proves G1 closed the streaming
// budget gap: a served stream whose upstream REPORTED usage is
// priced at the serving model and an overage is recorded on the
// request-budget feed — exactly like a non-streaming response.
// model-a prices $2.50/1M input + $10/1M output: the reported
// 12/6 split costs $0.00009, above the $0.00006 budget (the
// prompt-only pre-send estimate stays below it, so the request
// is served, not refused).
func TestStreamBudgetOverageRecorded(t *testing.T) {
	const sse = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18}}\n\n" +
		"data: [DONE]\n\n"
	up, _ := streamUsageUpstream(t, sse)
	client, err := provider.NewClient(up.URL, "upstream-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"p1": client}, nil, router.Config{
		MaxAttempts:      3,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "p1", Model: model}}
		},
	})
	srv := newTestServer(t, eng, WithPriceBook(testPriceBook()), WithTimeout(30*time.Second))
	rec := doEmbedReq(t, srv, "/v1/chat/completions",
		`{"model":"model-a","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123",
		map[string]string{"X-Codvian-Budget": "0.00006"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	feedRec := doReq(t, srv, http.MethodGet, "/dashboard/request-budgets", "", "")
	if feedRec.Code != http.StatusOK {
		t.Fatalf("feed status = %d", feedRec.Code)
	}
	var feed struct {
		Overages uint64 `json:"overages"`
		Events   []struct {
			Model         string   `json:"model"`
			Outcome       string   `json:"outcome"`
			ActualCostUSD *float64 `json:"actual_cost_usd"`
			PromptTokens  int      `json:"prompt_tokens"`
		} `json:"events"`
	}
	if err := json.Unmarshal(feedRec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("feed not JSON: %v", err)
	}
	if feed.Overages != 1 || len(feed.Events) != 1 {
		t.Fatalf("feed = %+v, want exactly 1 overage from the stream", feed)
	}
	ev := feed.Events[0]
	if ev.Outcome != "overage" || ev.Model != "model-a" {
		t.Fatalf("event = %+v, want overage on model-a", ev)
	}
	if ev.ActualCostUSD == nil || *ev.ActualCostUSD < 0.000089 || *ev.ActualCostUSD > 0.000091 {
		t.Errorf("actual cost = %v, want ~$0.00009 (12 in / 6 out at 2.5/10 per 1M)", ev.ActualCostUSD)
	}
	if ev.PromptTokens != 12 {
		t.Errorf("prompt tokens = %d, want the reported 12", ev.PromptTokens)
	}
}

// TestNonStreamingUsageProjection proves the client-facing usage
// block carries the OpenAI nested detail blocks (SDK parity) for
// both cached and reasoning tokens, alongside the flat counts —
// and never invents details the upstream did not report.
func TestNonStreamingUsageProjection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","model":"gate-model","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"prompt_tokens_details":{"cached_tokens":9},"completion_tokens_details":{"reasoning_tokens":5}}}`))
	}))
	t.Cleanup(up.Close)
	srv := streamUsageServer(t, up.URL)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"gate-model","messages":[{"role":"user","content":"hi"}]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Usage struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			CachedTokens        int `json:"cached_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	u := resp.Usage
	if u.PromptTokens != 20 || u.CompletionTokens != 8 || u.TotalTokens != 28 {
		t.Fatalf("usage = %+v, want 20/8/28", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 9 {
		t.Errorf("prompt_tokens_details = %+v, want cached_tokens 9", u.PromptTokensDetails)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Errorf("completion_tokens_details = %+v, want reasoning_tokens 5", u.CompletionTokensDetails)
	}
}

func TestProjectClientUsageNoInventedDetails(t *testing.T) {
	b := projectClientUsage(provider.Usage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4})
	if b.PromptTokensDetails != nil || b.CompletionTokensDetails != nil {
		t.Fatalf("details invented without a report: %+v", b)
	}
	if b.CachedTokens != 0 {
		t.Fatalf("cached = %d, want 0", b.CachedTokens)
	}
	z := projectClientUsage(provider.Usage{})
	if z.PromptTokens != 0 || z.TotalTokens != 0 || z.PromptTokensDetails != nil {
		t.Fatalf("zero usage projected wrong: %+v", z)
	}
	// P1-09: negative counts are malformed upstream output — the
	// client never sees negative tokens or a negative detail.
	n := projectClientUsage(provider.Usage{PromptTokens: -10, CompletionTokens: -2, TotalTokens: -12, CachedTokens: -3, ReasoningTokens: -1})
	if n.PromptTokens != 0 || n.CompletionTokens != 0 || n.TotalTokens != 0 || n.CachedTokens != 0 {
		t.Fatalf("negative usage projected wrong: %+v", n)
	}
	if n.PromptTokensDetails != nil || n.CompletionTokensDetails != nil {
		t.Fatalf("negative details invented: %+v", n)
	}
}
