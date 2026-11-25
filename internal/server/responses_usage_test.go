package server

// Phase 2 carry-over closure (addendum §7.1): the Responses API streaming
// path uses the same engine chunk-stream entry as chat completions, so the
// G1 accounting must reach BOTH the client-facing response.completed event
// AND the ledger with the provider-reported split. Before this slice that
// was true by construction but unproven, and a stale comment claimed the
// drain carried no usage.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// respUsageLedger captures what the ENGINE records (router.Ledger is a
// narrow interface; the concrete ledger has its own wider API).
type respUsageLedger struct {
	mu   sync.Mutex
	rows []provider.Usage
}

func (l *respUsageLedger) RecordAttempt(providerID, model string, ok bool, latencyMs int64, u provider.Usage) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, u)
}

func TestResponsesStreamUsageReachesClientAndLedger(t *testing.T) {
	const sse = "data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chatcmpl-x\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(up.Close)

	client, err := provider.NewClient(up.URL, "upstream-key", provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	led := &respUsageLedger{}
	eng := router.NewEngine(map[string]provider.Client{"p1": client}, led, router.Config{
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
	srv := newTestServer(t, eng, WithTimeout(30*time.Second))

	rec := doReq(t, srv, http.MethodPost, "/v1/responses",
		`{"model":"m","input":"hi","stream":true}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("not a completed responses stream:\n%s", body)
	}
	// The completed event carries the provider-reported usage block.
	completed := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: {") && strings.Contains(line, `"type":"response.completed"`) {
			completed = strings.TrimPrefix(line, "data: ")
		}
	}
	if completed == "" {
		t.Fatalf("no response.completed data line:\n%s", body)
	}
	var comp struct {
		Response struct {
			Usage *struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
				TotalTokens  int `json:"total_tokens"`
				InputDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"input_tokens_details,omitempty"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(completed), &comp); err != nil {
		t.Fatalf("completed not JSON: %v", err)
	}
	u := comp.Response.Usage
	if u == nil {
		t.Fatal("response.completed carries no usage")
	}
	if u.InputTokens != 12 || u.OutputTokens != 6 || u.TotalTokens != 18 {
		t.Errorf("completed usage = %+v, want 12/6/18 (reported)", u)
	}

	// The engine recorded the reported split (not an estimate).
	led.mu.Lock()
	rows := append([]provider.Usage(nil), led.rows...)
	led.mu.Unlock()
	if len(rows) == 0 {
		t.Fatal("no ledger rows for the responses stream")
	}
	row := rows[len(rows)-1]
	if row.PromptTokens != 12 || row.CompletionTokens != 6 || row.TotalTokens != 18 {
		t.Fatalf("ledger usage = %+v, want the reported 12/6/18", row)
	}
	if row.CachedTokens != 4 || row.ReasoningTokens != 2 {
		t.Errorf("ledger details = cached %d / reasoning %d, want 4 / 2", row.CachedTokens, row.ReasoningTokens)
	}
	if row.LocalEstimate {
		t.Error("responses stream recorded a local estimate despite an upstream report")
	}
}
