package provider

// G1 (streaming usage accounting) client-level regression tests:
// the OpenAI-wire stream request asks for include_usage; the final
// usage chunk (and its cached/reasoning splits and reported cost)
// is aggregated into the reassembled response; duplicate usage
// events take the LAST report, never a sum; no report stays zero.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamUsageMock serves a scripted SSE body for streaming
// requests and a plain JSON completion for non-streaming ones,
// recording the last request body it saw.
func streamUsageMock(t *testing.T, sse string) (*httptest.Server, *[]byte) {
	t.Helper()
	var lastBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastBody, _ = io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(lastBody, &probe)
		if !probe.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-x","model":"m","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody
}

func streamUsageReq() ChatRequest {
	return ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}}
}

func TestOpenAIStreamRequestsIncludeUsage(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	srv, body := streamUsageMock(t, sse)
	c, err := NewClient(srv.URL, "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cs, ok := c.(ChunkStreamer)
	if !ok {
		t.Fatal("openAI client does not implement ChunkStreamer")
	}
	if _, err := cs.ChatStreamChunks(context.Background(), streamUsageReq(), nil); err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(*body, &req); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if req["stream"] != true {
		t.Fatalf("stream = %v, want true", req["stream"])
	}
	so, ok := req["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options = %v, want include_usage:true", req["stream_options"])
	}

	// Non-streaming requests never carry the block.
	if _, err := c.Chat(context.Background(), streamUsageReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var req2 map[string]any
	if err := json.Unmarshal(*body, &req2); err != nil {
		t.Fatalf("request body not JSON: %v", err)
	}
	if _, present := req2["stream_options"]; present {
		t.Fatalf("non-streaming request carries stream_options: %v", req2)
	}
}

func TestOpenAIStreamUsageAggregation(t *testing.T) {
	// Two usage events: the LAST one wins (never summed). The
	// final one carries the cached/reasoning splits and a
	// reported cost, exactly like OpenRouter's usage chunk.
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":6,\"total_tokens\":18,\"prompt_tokens_details\":{\"cached_tokens\":4},\"completion_tokens_details\":{\"reasoning_tokens\":2},\"cost\":0.00031}}\n\n" +
		"data: [DONE]\n\n"
	srv, _ := streamUsageMock(t, sse)
	c, err := NewClient(srv.URL, "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cs := c.(ChunkStreamer)
	var chunks []StreamChunk
	resp, err := cs.ChatStreamChunks(context.Background(), streamUsageReq(), func(ch StreamChunk) error {
		chunks = append(chunks, ch)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "hi" || resp.FinishReason != "stop" {
		t.Fatalf("resp = %+v, want content hi / finish stop", resp)
	}
	u := resp.Usage
	if u.PromptTokens != 12 || u.CompletionTokens != 6 || u.TotalTokens != 18 {
		t.Errorf("usage = %+v, want the LAST report 12/6/18 (never a sum)", u)
	}
	if u.CachedTokens != 4 || u.ReasoningTokens != 2 {
		t.Errorf("usage details = %+v, want cached 4 / reasoning 2", u)
	}
	if u.Cost == nil || *u.Cost != 0.00031 {
		t.Errorf("Cost = %v, want the reported 0.00031", u.Cost)
	}
	// Usage-only chunks (choices:[]) update the aggregate but are
	// not content events — the reassembled response above is the
	// authoritative carrier (the engine reads it from Done.Resp).
	// A usage block riding a choice-carrying chunk IS delivered.
	if len(chunks) == 0 {
		t.Error("no chunks delivered to onChunk")
	}
}

func TestOpenAIStreamNoUsageStaysZero(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	srv, _ := streamUsageMock(t, sse)
	c, err := NewClient(srv.URL, "k")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	resp, err := c.(ChunkStreamer).ChatStreamChunks(context.Background(), streamUsageReq(), nil)
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Usage != (Usage{}) {
		t.Fatalf("usage = %+v, want zero (no report, never invented)", resp.Usage)
	}
}

func TestUsageNonNegative(t *testing.T) {
	u := Usage{
		PromptTokens: -10, CompletionTokens: 5, TotalTokens: -5,
		CachedTokens: -2, ReasoningTokens: -1, CacheWriteTokens: -3,
		CacheWriteTokens5m: -1, CacheWriteTokens1h: 2,
	}
	got := u.NonNegative()
	if got.PromptTokens != 0 || got.CompletionTokens != 5 || got.TotalTokens != 0 ||
		got.CachedTokens != 0 || got.ReasoningTokens != 0 || got.CacheWriteTokens != 0 ||
		got.CacheWriteTokens5m != 0 || got.CacheWriteTokens1h != 2 {
		t.Fatalf("NonNegative = %+v, want negatives clamped, valid counts kept", got)
	}
	// The receiver is a value copy: the original is untouched.
	if u.PromptTokens != -10 {
		t.Fatalf("NonNegative mutated its receiver: %+v", u)
	}
	valid := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CachedTokens: 3}
	if valid.NonNegative() != valid {
		t.Fatal("NonNegative changed a valid report")
	}
}

func TestChatRequestStreamOptionsDecode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
		err  bool
	}{
		{"include_usage true", `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, true, false},
		{"include_usage false", `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`, false, false},
		{"absent", `{"model":"m","stream":true}`, false, false},
		{"null block", `{"model":"m","stream":true,"stream_options":null}`, false, false},
		{"empty block", `{"model":"m","stream":true,"stream_options":{}}`, false, false},
		{"unknown key ignored", `{"model":"m","stream":true,"stream_options":{"include_usage":true,"future_flag":1}}`, true, false},
		{"malformed value", `{"model":"m","stream":true,"stream_options":{"include_usage":"yes"}}`, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req ChatRequest
			err := json.Unmarshal([]byte(tc.body), &req)
			if tc.err {
				if err == nil || !strings.Contains(err.Error(), "stream_options") {
					t.Fatalf("err = %v, want a stream_options decode error", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if req.StreamIncludeUsage != tc.want {
				t.Fatalf("StreamIncludeUsage = %v, want %v", req.StreamIncludeUsage, tc.want)
			}
		})
	}
}
