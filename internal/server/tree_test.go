package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// TestChatCompletionsStrategyTree drives one real request through
// a tree-configured engine (CHECKLIST 177): the tree
// cheapest-of[fastest-of[a, b], c] serves c first, the response
// carries X-Codvian-Strategy: cheapest, and the strategies feed
// + route trace both name the tree path to the leaf that served
// it. The mocks are real HTTP upstreams behind real provider
// clients, like the failover gate test.
func TestChatCompletionsStrategyTree(t *testing.T) {
	var aHits, bHits, cHits atomic.Int32
	mock := func(name string, hits *atomic.Int32, content string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-` + name + `","object":"chat.completion","model":"tree-model","choices":[{"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
		}))
	}
	aSrv := mock("a", &aHits, "from-a")
	defer aSrv.Close()
	bSrv := mock("b", &bHits, "from-b")
	defer bSrv.Close()
	cSrv := mock("c", &cHits, "from-c")
	defer cSrv.Close()

	clients := map[string]provider.Client{}
	for id, srv := range map[string]*httptest.Server{"a": aSrv, "b": bSrv, "c": cSrv} {
		cl, err := provider.NewClient(srv.URL, id+"-key", provider.WithTimeout(2*time.Second))
		if err != nil {
			t.Fatalf("NewClient %s: %v", id, err)
		}
		clients[id] = cl
	}

	tree := &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
		{Strategy: "fastest", Children: []*strategytree.Node{
			{Provider: "a", Model: "tree-model"},
			{Provider: "b", Model: "tree-model"},
		}},
		{Provider: "c", Model: "tree-model"},
	}}
	// Deterministic strategy hook for the test (costs a=10 b=1
	// c=5, latencies a=5 b=50 c=10): cheapest-of[fastest-of[a,b],
	// c] orders c, a, b.
	hook := func(strategy string) func([]router.Attempt) []router.Attempt {
		return func(chain []router.Attempt) []router.Attempt {
			out := append([]router.Attempt(nil), chain...)
			key := map[string]float64{"a": 10, "b": 1, "c": 5}
			if strategy == "fastest" || strategy == "least-latency" {
				key = map[string]float64{"a": 5, "b": 50, "c": 10}
			}
			for i := 1; i < len(out); i++ {
				for j := i; j > 0 && key[out[j].ProviderID] < key[out[j-1].ProviderID]; j-- {
					out[j], out[j-1] = out[j-1], out[j]
				}
			}
			return out
		}
	}
	eng := router.NewEngine(clients, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1, // hedging off: deterministic attempt order
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		StrategyHook:     hook,
		StrategyTree:     tree,
		ChainFor: func(model string) []router.Attempt {
			if model != "tree-model" {
				return nil
			}
			return []router.Attempt{{ProviderID: "a", Model: "tree-model"}, {ProviderID: "b", Model: "tree-model"}, {ProviderID: "c", Model: "tree-model"}}
		},
	})
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Routing = &config.RoutingConfig{StrategyTree: tree}
	key, rawKey, err := config.IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	cfg.GatewayKeys = []config.GatewayKey{key}
	srv := newTestServer(t, eng, WithConfig(cfg), WithTimeout(10*time.Second))

	resp := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		`{"model":"tree-model","messages":[{"role":"user","content":"hi"}]}`, rawKey)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", resp.Code, resp.Body.String())
	}
	var out struct {
		Choices []struct {
			Message provider.Message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, resp.Body.String())
	}
	if len(out.Choices) != 1 || out.Choices[0].Message.Content != "from-c" {
		t.Fatalf("response = %+v, want content from-c (cheapest winner serves first)", out)
	}
	if got := aHits.Load(); got != 0 {
		t.Fatalf("mock a hits = %d, want 0 (the tree winner serves first, no fallback needed)", got)
	}
	if got := bHits.Load(); got != 0 {
		t.Fatalf("mock b hits = %d, want 0", got)
	}
	if got := cHits.Load(); got != 1 {
		t.Fatalf("mock c hits = %d, want 1", got)
	}
	if got := resp.Header().Get("X-Codvian-Strategy"); got != "cheapest" {
		t.Fatalf("X-Codvian-Strategy = %q, want cheapest (the tree root's strategy)", got)
	}

	// The strategies feed renders the configured tree verbatim
	// and names the leaf that served the last request.
	feed := getStrategies(t, srv)
	if feed.StrategyTree == nil || feed.StrategyTree.Strategy != "cheapest" {
		t.Fatalf("strategies feed strategy_tree = %+v, want the configured cheapest tree", feed.StrategyTree)
	}
	if len(feed.StrategyTree.Children) != 2 || feed.StrategyTree.Children[0].Strategy != "fastest" {
		t.Fatalf("strategies feed strategy_tree children = %+v, want [fastest-of[a,b], c]", feed.StrategyTree.Children)
	}
	if feed.Last == nil || feed.Last.Provider != "c" {
		t.Fatalf("strategies feed last = %+v, want provider c", feed.Last)
	}
	if got := strings.Join(feed.LastTreePath, " > "); got != "cheapest > c" {
		t.Fatalf("strategies feed last_tree_path = %v, want [cheapest c]", feed.LastTreePath)
	}

	// The route trace carries the same path.
	rec := doReq(t, srv, http.MethodGet, "/dashboard/trace", "", rawKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/trace status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var trace struct {
		StrategyTreePath []string `json:"strategy_tree_path"`
		Attempts         []struct {
			Provider string `json:"provider"`
			Outcome  string `json:"outcome"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &trace); err != nil {
		t.Fatalf("decode trace: %v (body %s)", err, rec.Body.String())
	}
	if got := strings.Join(trace.StrategyTreePath, " > "); got != "cheapest > c" {
		t.Fatalf("trace strategy_tree_path = %v, want [cheapest c]", trace.StrategyTreePath)
	}
	if len(trace.Attempts) != 1 || trace.Attempts[0].Provider != "c" || trace.Attempts[0].Outcome != "success" {
		t.Fatalf("trace attempts = %+v, want exactly one successful attempt by c", trace.Attempts)
	}
}
