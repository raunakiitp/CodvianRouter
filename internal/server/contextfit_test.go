package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/tokencount"
)

// contextFitMock is one OpenAI-compatible mock upstream for the
// context-fit server tests. It counts hits and answers every chat
// completion with its own content; with stream=true it answers
// with a minimal SSE stream instead.
type contextFitMock struct {
	hits   atomic.Int32
	stream atomic.Int32
	body   string // content returned for non-streaming completions
	chunk  string // single text chunk returned for streams
}

func (m *contextFitMock) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		m.hits.Add(1)
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Stream {
			m.stream.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", m.chunk)
			if flusher != nil {
				flusher.Flush()
			}
			fmt.Fprintf(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprintf(w, "data: [DONE]\n\n")
			return
		}
		fmt.Fprintf(w, `{"id":"chatcmpl-x","object":"chat.completion","model":"gate-model","choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`, m.body)
	}
}

// contextFitClients starts both mock upstreams and returns their
// provider clients. The servers close when the test ends.
func contextFitClients(t *testing.T, small, long *contextFitMock) (provider.Client, provider.Client) {
	t.Helper()
	smallSrv := httptest.NewServer(small.handler())
	longSrv := httptest.NewServer(long.handler())
	t.Cleanup(func() { smallSrv.Close(); longSrv.Close() })
	smallClient, err := provider.NewClient(smallSrv.URL, "small-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient small: %v", err)
	}
	longClient, err := provider.NewClient(longSrv.URL, "long-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient long: %v", err)
	}
	return smallClient, longClient
}

// contextFitEngine builds a real router.Engine over the two mock
// clients — "small" (8k window) and "long" (longWindow) — with the
// fit check wired from the window map and the dedicated
// context-window fallback chain pointing at "long".
func contextFitEngine(smallClient, longClient provider.Client, longWindow int) *router.Engine {
	return router.NewEngine(map[string]provider.Client{"small": smallClient, "long": longClient}, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			if model != "gate-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "small", Model: "gate-model"}}
		},
		ContextWindow: func(providerID, model string) (int, bool) {
			switch providerID {
			case "small":
				return 8192, true
			case "long":
				return longWindow, true
			}
			return 0, false
		},
		ContextFallbackChain: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "long", Model: "gate-model"}}
		},
	})
}

// contextFitPrompt builds a prompt of at least targetTokens local
// tokens (measured with the same counter the engine uses).
// Tokenization is not linear across repetitions — a repeated
// sentence can merge tokens at the joins — so the prompt is
// measured after every build and extended until it really reaches
// the target, never assumed from the unit count.
func contextFitPrompt(t *testing.T, targetTokens int) string {
	t.Helper()
	unit := "The quick brown fox jumps over the lazy dog while the river runs cold and clear. "
	one, err := tokencount.Count("gate-model", unit)
	if err != nil {
		t.Fatalf("count unit: %v", err)
	}
	if one.Tokens <= 0 {
		t.Fatalf("unit prompt counted %d tokens", one.Tokens)
	}
	reps := targetTokens/one.Tokens + 1
	for {
		prompt := strings.Repeat(unit, reps)
		got, err := tokencount.CountPrompt("gate-model", []provider.Message{{Role: "user", Content: prompt}})
		if err != nil {
			t.Fatalf("count prompt: %v", err)
		}
		if got.Tokens >= targetTokens {
			return prompt
		}
		// Top up by the measured deficit, not by another full
		// target — the first build already gets close.
		reps += (targetTokens-got.Tokens)/one.Tokens + 1
	}
}

func contextFitChatBody(prompt string, stream bool) string {
	body, _ := json.Marshal(map[string]any{
		"model":  "gate-model",
		"stream": stream,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
	})
	return string(body)
}

// TestChatCompletionsContextFitJumpsToFallback is the server-level
// gate (CHECKLIST 159): one POST through the real Server + real
// Engine + real provider clients — an oversize prompt is served by
// the long-context model, and the small mock is never hit.
func TestChatCompletionsContextFitJumpsToFallback(t *testing.T) {
	small, long := &contextFitMock{body: "small says hi"}, &contextFitMock{body: "long says hi"}
	smallClient, longClient := contextFitClients(t, small, long)
	eng := contextFitEngine(smallClient, longClient, 1048576)
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))

	prompt := contextFitPrompt(t, 6000) // required ~10,096 > 8,192
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", contextFitChatBody(prompt, false), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "long says hi") {
		t.Fatalf("body = %s, want the long-context model's reply", rec.Body.String())
	}
	if got := small.hits.Load(); got != 0 {
		t.Fatalf("small mock hits = %d, want 0 (never dialled)", got)
	}
	if got := long.hits.Load(); got != 1 {
		t.Fatalf("long mock hits = %d, want 1", got)
	}
	// The trace records the skip honestly, with the fit decision.
	rec = doReq(t, srv, http.MethodGet, "/dashboard/trace", "", "")
	var trace struct {
		Attempts []struct {
			Provider string `json:"provider"`
			Outcome  string `json:"outcome"`
		} `json:"attempts"`
		ContextFit *struct {
			ChainUsed string `json:"chain_used"`
		} `json:"context_fit"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatalf("decode trace: %v", err)
	}
	if len(trace.Attempts) != 2 || trace.Attempts[0].Outcome != "skipped_context_fit" || trace.Attempts[0].Provider != "small" {
		t.Fatalf("trace attempts = %+v, want [small skipped_context_fit, long success]", trace.Attempts)
	}
	if trace.ContextFit == nil || trace.ContextFit.ChainUsed != "context_fallback" {
		t.Fatalf("trace context_fit = %+v, want chain_used context_fallback", trace.ContextFit)
	}
}

// TestChatCompletionsContextFitNoFitIs400 proves the honest
// no-fit failure at the HTTP layer: a prompt bigger than every
// configured window answers 400 naming required vs largest, with
// zero provider calls.
func TestChatCompletionsContextFitNoFitIs400(t *testing.T) {
	small, long := &contextFitMock{body: "small says hi"}, &contextFitMock{body: "long says hi"}
	smallClient, longClient := contextFitClients(t, small, long)
	// Long window 16k: the fallback chain fits nothing either.
	eng := contextFitEngine(smallClient, longClient, 16384)
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))

	prompt := contextFitPrompt(t, 20000) // required ~24,096 > 16,384
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", contextFitChatBody(prompt, false), "test-key-123")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "largest available context window is 16384") {
		t.Fatalf("body = %s, want the required-vs-largest message", rec.Body.String())
	}
	if got := small.hits.Load(); got != 0 {
		t.Fatalf("small mock hits = %d, want 0", got)
	}
	if got := long.hits.Load(); got != 0 {
		t.Fatalf("long mock hits = %d, want 0", got)
	}
}

// TestChatCompletionsContextFitStreaming proves the streaming path
// jumps to the long-context model too: the small mock's stream is
// never opened, and the long model's chunk reaches the client.
func TestChatCompletionsContextFitStreaming(t *testing.T) {
	small, long := &contextFitMock{body: "small says hi", chunk: "small chunk"}, &contextFitMock{body: "long says hi", chunk: "long chunk"}
	smallClient, longClient := contextFitClients(t, small, long)
	eng := contextFitEngine(smallClient, longClient, 1048576)
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))

	prompt := contextFitPrompt(t, 6000)
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", contextFitChatBody(prompt, true), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "long chunk") {
		t.Fatalf("body = %s, want the long model's stream chunk", rec.Body.String())
	}
	if got := small.stream.Load(); got != 0 {
		t.Fatalf("small mock streams = %d, want 0 (never dialled)", got)
	}
	if got := long.stream.Load(); got != 1 {
		t.Fatalf("long mock streams = %d, want 1", got)
	}
}

// TestDashboardContextFitFeed proves the standalone feed and the
// empty state: has_decision false before any request, true (with
// the real decision) after one.
func TestDashboardContextFitFeed(t *testing.T) {
	small, long := &contextFitMock{body: "small says hi"}, &contextFitMock{body: "long says hi"}
	smallClient, longClient := contextFitClients(t, small, long)
	eng := contextFitEngine(smallClient, longClient, 1048576)
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))

	rec := doReq(t, srv, http.MethodGet, "/dashboard/context-fit", "", "")
	var empty struct {
		HasDecision bool `json:"has_decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode empty feed: %v", err)
	}
	if empty.HasDecision {
		t.Fatalf("feed = %+v, want has_decision false before any request", empty)
	}

	prompt := contextFitPrompt(t, 6000)
	if rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions", contextFitChatBody(prompt, false), "test-key-123"); rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/context-fit", "", "")
	var feed struct {
		HasDecision bool `json:"has_decision"`
		Decision    *struct {
			Model          string `json:"model"`
			PromptTokens   int    `json:"prompt_tokens"`
			ReserveTokens  int    `json:"reserve_tokens"`
			RequiredTokens int    `json:"required_tokens"`
			ChainUsed      string `json:"chain_used"`
			LargestWindow  int    `json:"largest_window"`
			Entries        []struct {
				Provider string `json:"provider"`
				Fits     bool   `json:"fits"`
			} `json:"entries"`
		} `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if !feed.HasDecision || feed.Decision == nil {
		t.Fatalf("feed = %+v, want a decision after one request", feed)
	}
	d := feed.Decision
	if d.Model != "gate-model" || d.ChainUsed != "context_fallback" {
		t.Fatalf("decision = %+v, want model gate-model, chain_used context_fallback", d)
	}
	if d.RequiredTokens != d.PromptTokens+d.ReserveTokens || d.PromptTokens < 6000 || d.ReserveTokens != router.DefaultContextReserveTokens {
		t.Fatalf("decision tokens = %+v, want prompt >= 6000, reserve %d, required = prompt + reserve", d, router.DefaultContextReserveTokens)
	}
	if d.LargestWindow != 1048576 || len(d.Entries) != 2 || d.Entries[0].Fits || !d.Entries[1].Fits {
		t.Fatalf("decision entries = %+v, want [small too-small, long fits], largest 1048576", d)
	}
}
