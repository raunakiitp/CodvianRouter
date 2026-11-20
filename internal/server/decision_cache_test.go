// Decision header on a cache hit (CHECKLIST 199). This test lives
// in package server_test (not the in-package decision_test.go):
// the degrade wrapper imports the server package, so an in-package
// test cannot import it. The fixture mirrors the production wiring
// (StaleEngine with ServeCache on) and the compression-cache tests.
package server_test

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/degrade"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/server"
)

type decisionCacheLedger struct{}

func (decisionCacheLedger) RecordAttempt(providerID, model string, ok bool, _ int64, _ provider.Usage) {
}

func TestDecisionHeaderAbsentOnCacheHit(t *testing.T) {
	var chatHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"dec-model","choices":[{"message":{"role":"assistant","content":"cached answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()
	client, err := provider.NewClient(up.URL, "test-key", provider.WithTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"mock": client}, decisionCacheLedger{}, router.Config{
		Strategy: "cheapest", MaxAttempts: 5, HedgeDelay: -1,
		BackoffBase: time.Millisecond, BackoffMax: 5 * time.Millisecond,
		BreakerThreshold: 5, BreakerOpenFor: time.Minute, RetryAfterCap: 30 * time.Second,
		ChainFor: func(m string) []router.Attempt {
			if m != "dec-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "mock", Model: "dec-model"}}
		},
	})
	serving := degrade.WrapWithSemantic(eng, cache.NewExactCache(500), cache.NewLocalSemanticCache(), degrade.Policy{
		Enabled: true, AllowStale: true, ServeCache: true,
	})
	srv := server.New(serving,
		server.WithStaticKeys("test-key-123"), server.WithTimeout(10*time.Second),
		server.WithLogger(log.New(io.Discard, "", 0)))
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-key-123")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	body := `{"model":"dec-model","messages":[{"role":"user","content":"same question"}]}`
	first := post(body)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("X-Codvian-Decision"); got != "provider=mock; model=dec-model; strategy=cheapest; reason=cheapest" {
		t.Errorf("first X-Codvian-Decision = %q, want the provider decision", got)
	}
	if got := chatHits.Load(); got != 1 {
		t.Fatalf("mock chat hits after first = %d, want 1", got)
	}
	second := post(body)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	// Served from cache: no provider was called for THIS response,
	// so there is no decision to name — X-Codvian-Cache names the
	// layer instead, and the mock was not hit again. The header
	// must be absent even though the engine's LastRouting still
	// holds the first request's record.
	if got := second.Header().Get("X-Codvian-Decision"); got != "" {
		t.Errorf("second X-Codvian-Decision = %q, want absent (cache served it)", got)
	}
	if got := second.Header().Get("X-Codvian-Cache"); got == "" {
		t.Errorf("X-Codvian-Cache absent on the cached response")
	}
	if got := chatHits.Load(); got != 1 {
		t.Errorf("mock chat hits after second = %d, want still 1 (cache served it)", got)
	}
	var out struct {
		Choices []struct {
			Message provider.Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &out); err != nil || len(out.Choices) != 1 || out.Choices[0].Message.Content != "cached answer" {
		t.Fatalf("second body = %s, want the cached answer", second.Body.String())
	}
}
