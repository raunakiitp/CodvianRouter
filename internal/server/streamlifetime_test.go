// Stream-lifetime server tests (CHECKLIST 255-256): the live
// feed, the SSE terminal frame naming the cap, and the request
// validation for reasoning_effort.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/metrics"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// nonReportingEngine is an engine that does not report a
// stream-lifetime cap — the feed must answer the honest disabled
// state for it, never an invented cap.
type nonReportingEngine struct{}

func (nonReportingEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (nonReportingEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func lifetimeFeed(t *testing.T, srv *Server) streamLifetimeResponse {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/dashboard/stream-lifetime", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/stream-lifetime status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp streamLifetimeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode stream-lifetime: %v (body %s)", err, rec.Body.String())
	}
	return resp
}

// TestServerStreamLifetimeFeed: the feed reports the engine's
// effective cap and live kill count; a disabled cap and a
// non-reporting engine both report enabled=false honestly.
func TestServerStreamLifetimeFeed(t *testing.T) {
	// Real engine, 5-minute cap, nothing killed yet.
	eng := router.NewEngine(map[string]provider.Client{"a": &fakeEngineClient{}}, nil, router.Config{
		StreamMaxLifetime: 5 * time.Minute,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: model}}
		},
	})
	srv := newTestServer(t, eng)
	resp := lifetimeFeed(t, srv)
	if !resp.Enabled || resp.MaxLifetimeMs != 300000 || resp.MaxLifetime != "5m0s" || resp.KilledTotal != 0 {
		t.Errorf("feed = %+v, want enabled, 300000ms, 5m0s, 0 killed", resp)
	}

	// Disabled cap (negative) reports enabled=false.
	disabled := router.NewEngine(map[string]provider.Client{"a": &fakeEngineClient{}}, nil, router.Config{
		StreamMaxLifetime: -1,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: model}}
		},
	})
	resp = lifetimeFeed(t, newTestServer(t, disabled))
	if resp.Enabled || resp.MaxLifetimeMs != 0 || resp.MaxLifetime != "0s" {
		t.Errorf("disabled feed = %+v, want enabled=false, 0ms, 0s", resp)
	}

	// An engine that does not report a lifetime at all answers
	// the same honest disabled state, never an invented cap.
	resp = lifetimeFeed(t, newTestServer(t, nonReportingEngine{}))
	if resp.Enabled || resp.MaxLifetimeMs != 0 || resp.KilledTotal != 0 {
		t.Errorf("non-reporting feed = %+v, want enabled=false, 0ms, 0 killed", resp)
	}
}

// fakeEngineClient is a minimal provider.Client for engine-only
// server tests (no HTTP mock needed).
type fakeEngineClient struct{}

func (fakeEngineClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (fakeEngineClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	if onDelta != nil {
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}

func (fakeEngineClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (fakeEngineClient) TestKey(ctx context.Context) error                { return nil }

// TestServerStreamLifetimeSSECap: a held upstream stream (one
// chunk, then silence) is killed at a short engine cap, and the
// client sees the streamed chunk, then an SSE error event naming
// the cap, then [DONE] — never a hang, never a bare close. The
// request runs in a goroutine with a 5 s deadline so a missing
// cap fails the test instead of hanging it.
func TestServerStreamLifetimeSSECap(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not flush")
			return
		}
		fmt.Fprintf(w, "data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"held-\"},\"finish_reason\":null}]}\n\n")
		fl.Flush()
		// Hold the stream open forever: no more events, no
		// close — only the engine's lifetime cap ends it.
		<-r.Context().Done()
	}))
	defer mock.Close()
	cli, err := provider.NewClient(mock.URL, "k", provider.WithTimeout(10*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"a": cli}, nil, router.Config{
		MaxAttempts:       5,
		HedgeDelay:        -1,
		BackoffBase:       time.Millisecond,
		BackoffMax:        5 * time.Millisecond,
		BreakerThreshold:  5,
		BreakerOpenFor:    time.Minute,
		RetryAfterCap:     30 * time.Second,
		StreamMaxLifetime: 200 * time.Millisecond,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: "m"}}
		},
	})
	srv := newTestServer(t, eng, WithTimeout(10*time.Second))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doReq(t, srv, http.MethodPost, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`, "test-key-123")
	}()
	select {
	case rec := <-done:
		body := rec.Body.String()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, body)
		}
		// The streamed chunk is "held-"; the stream redactor
		// (M1.7) holds a trailing "-" (a partial "-----begin"
		// prefix) for the next chunk and emits it as its own
		// event before the error frame, so the raw body never
		// contains the literal "held-". A client concatenating
		// deltas still sees "held-" — assert on the reassembled
		// text, not the raw body.
		var streamed strings.Builder
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err == nil {
				for _, c := range chunk.Choices {
					streamed.WriteString(c.Delta.Content)
				}
			}
		}
		if !strings.Contains(streamed.String(), "held-") {
			t.Errorf("body = %s, want the streamed chunk before the cap", body)
		}
		if !strings.Contains(body, "stream lifetime exceeded") {
			t.Errorf("body = %s, want the SSE error event naming the cap", body)
		}
		if !strings.Contains(body, "[DONE]") {
			t.Errorf("body = %s, want [DONE] after the error event (a client must never wait forever for the terminator)", body)
		}
		// The kill is counted live: the feed must now say 1.
		if resp := lifetimeFeed(t, srv); resp.KilledTotal != 1 {
			t.Errorf("feed killed_total = %d, want 1 after the capped stream", resp.KilledTotal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("streaming request did not return in 5 s — the cap did not kill the held stream")
	}
}

// TestServerValidateReasoningEffort: an unknown reasoning_effort
// is a 400 naming the valid levels; a known level passes
// validation and serves.
func TestServerValidateReasoningEffort(t *testing.T) {
	srv, _, _, key := groupStack(t)
	rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"bogus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reasoning_effort") {
		t.Errorf("body = %s, want the 400 to name reasoning_effort", rec.Body.String())
	}
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		rec := groupChat(t, srv, key, `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"`+level+`"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("reasoning_effort %q: status = %d, want 200 (body %s)", level, rec.Code, rec.Body.String())
		}
	}
}

// TestServerGroupsFeedEffort: the groups feed shows each
// (provider, effort) entry separately — the same provider twice
// at different efforts is two entries, not one deduped row.
func TestServerGroupsFeedEffort(t *testing.T) {
	m := newContractMock(t, "a")
	cli, err := provider.NewClient(m.server.URL, "a-key", provider.WithTimeout(2*time.Second), provider.WithAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	eng := router.NewEngine(map[string]provider.Client{"a": cli}, nil, router.Config{
		MaxAttempts:      5,
		HedgeDelay:       -1,
		BackoffBase:      time.Millisecond,
		BackoffMax:       5 * time.Millisecond,
		BreakerThreshold: 5,
		BreakerOpenFor:   time.Minute,
		RetryAfterCap:    30 * time.Second,
		ChainFor: func(model string) []router.Attempt {
			return []router.Attempt{{ProviderID: "a", Model: "m"}}
		},
	})
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Providers = []config.ProviderConfig{
		{ID: "a", Name: "Alpha", BaseURL: m.server.URL, Enabled: true},
	}
	cfg.Routing = &config.RoutingConfig{
		Groups: map[string][]string{"efforts": {"a@low", "a@high"}},
	}
	srv := newTestServer(t, eng, WithMetrics(metrics.NewRecorder()), WithTimeout(10*time.Second), WithConfig(cfg))
	resp := groupsFeed(t, srv)
	if len(resp.Groups) != 1 {
		t.Fatalf("groups = %+v, want one group", resp.Groups)
	}
	g := resp.Groups[0]
	if g.Name != "efforts" {
		t.Fatalf("group name = %q, want efforts", g.Name)
	}
	if len(g.Members) != 2 {
		t.Fatalf("members = %+v, want two entries (same provider, two efforts)", g.Members)
	}
	if g.Members[0].ProviderID != "a" || g.Members[0].Effort != "low" {
		t.Errorf("member[0] = %+v, want a/low", g.Members[0])
	}
	if g.Members[1].ProviderID != "a" || g.Members[1].Effort != "high" {
		t.Errorf("member[1] = %+v, want a/high", g.Members[1])
	}
}
