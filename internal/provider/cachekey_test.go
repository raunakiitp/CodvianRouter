package provider

// Auto prompt_cache_key (CHECKLIST 131-132; research-log entry 184):
// for requests to OpenAI's own API with no client-supplied key, the
// router adds "codvian-" + a keyed HMAC of the caller (gateway-key
// ID + endpoint) and the request's shared prefix (leading
// system/developer messages, plus tools). User messages are NOT
// part of it. A client-supplied key (including explicit null) is
// never replaced. Other OpenAI-compatible endpoints never get a
// generated key. The key is generated per attempt at send time and
// never used for routing or stored in history.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func sysUserReq() ChatRequest {
	return ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "system", Content: "You are a coding assistant."},
			{Role: "user", Content: "Write a function."},
		},
	}
}

func TestIsOpenAIAPI(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"https://api.openai.com", true},
		{"https://api.openai.com/v1", true},
		{"https://api.openai.com/v1/", true},
		{"https://API.OPENAI.COM", true},
		{"https://api.openai.com:443", true},
		{"https://api.openai.com.evil.com", false},
		{"https://notapi.openai.com", false},
		{"https://api.openai.comx", false},
		{"https://api.groq.com/openai/v1", false},
		{"https://api.codvian.com/v1", false},
		{"http://127.0.0.1:8080/v1", false},
		{"not a url", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsOpenAIAPI(tc.base); got != tc.want {
			t.Errorf("IsOpenAIAPI(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}

func TestAutoPromptCacheKeyRules(t *testing.T) {
	req := sysUserReq()
	key1, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, req)
	if !ok {
		t.Fatal("want a key for a caller + system prefix")
	}
	if !strings.HasPrefix(key1, "codvian-") {
		t.Fatalf("key %q lacks the codvian- prefix", key1)
	}
	// Same caller + same prefix -> same key (deterministic).
	key2, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, req)
	if !ok || key2 != key1 {
		t.Fatalf("same caller+prefix gave different keys: %q vs %q", key1, key2)
	}
	// Different caller -> different key.
	key3, _ := AutoPromptCacheKey("gk-bob", CacheKeyEndpointChatCompletions, req)
	if key3 == key1 {
		t.Fatal("different callers got the same key")
	}
	// Different endpoint label -> different key.
	key4, _ := AutoPromptCacheKey("gk-alice", "responses", req)
	if key4 == key1 {
		t.Fatal("different endpoints got the same key")
	}
	// User messages are NOT part of the key: only the user turn
	// differs, so the key must be identical.
	otherUser := sysUserReq()
	otherUser.Messages[1].Content = "A completely different question."
	key5, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, otherUser)
	if !ok || key5 != key1 {
		t.Fatalf("user message changed the key: %q vs %q", key5, key1)
	}
	// A different system prompt -> a different key.
	otherSys := sysUserReq()
	otherSys.Messages[0].Content = "You are a poet."
	key6, _ := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, otherSys)
	if key6 == key1 {
		t.Fatal("different system prompt got the same key")
	}
	// Tools are part of the prefix: same messages, different tools.
	withTools := sysUserReq()
	withTools.Tools = []ToolDefinition{{
		Type: "function",
		Function: struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
		}{Name: "get_weather"},
	}}
	key7, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, withTools)
	if !ok {
		t.Fatal("want a key when tools are present")
	}
	if key7 == key1 {
		t.Fatal("different tools got the same key")
	}
	// Tools alone (no system/developer messages) still anchor a key.
	toolsOnly := ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    withTools.Tools,
	}
	if _, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, toolsOnly); !ok {
		t.Fatal("want a key for a tools-only request")
	}
	// No instructions and no tools -> no key.
	bare := ChatRequest{
		Model:    "gpt-4o",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	if _, ok := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, bare); ok {
		t.Fatal("bare user-only request must get no key")
	}
	// No caller -> no key (callers must never share one).
	if _, ok := AutoPromptCacheKey("", CacheKeyEndpointChatCompletions, req); ok {
		t.Fatal("anonymous caller must get no key")
	}
	// Leading system/developer messages only: a developer message
	// after a user turn is NOT part of the shared prefix.
	mixed := ChatRequest{
		Model: "gpt-4o",
		Messages: []Message{
			{Role: "system", Content: "You are a coding assistant."},
			{Role: "user", Content: "hi"},
			{Role: "developer", Content: "Be terse."},
		},
	}
	key8, _ := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, mixed)
	if key8 != key1 {
		t.Fatalf("trailing developer message changed the key: %q vs %q", key8, key1)
	}
	// JSON key order in the decoded request must not matter: the
	// prefix is re-marshalled from decoded structs.
	raw := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"Write a function."}]}`
	var decoded ChatRequest
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	key9, _ := AutoPromptCacheKey("gk-alice", CacheKeyEndpointChatCompletions, decoded)
	if key9 != key1 {
		t.Fatalf("decoded request gave a different key: %q vs %q", key9, key1)
	}
}

// rewriteTransport rewrites every request's scheme+host to a local
// mock while keeping the original URL's path: the client believes it
// is talking to api.openai.com (so the auto-key rule fires) while
// the bytes land on a recording httptest server.
type rewriteTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = t.target.Scheme
	clone.URL.Host = t.target.Host
	return t.base.RoundTrip(clone)
}

type recordedBody struct {
	raw map[string]json.RawMessage
}

func recordMock(t *testing.T, usage string) (*httptest.Server, *[]recordedBody) {
	t.Helper()
	var bodies []recordedBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		bodies = append(bodies, recordedBody{raw: raw})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-x","object":"chat.completion","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":` + usage + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func openAIClientViaMock(t *testing.T, mock *httptest.Server) Client {
	t.Helper()
	target, err := url.Parse(mock.URL)
	if err != nil {
		t.Fatalf("parse mock URL: %v", err)
	}
	hc := &http.Client{
		Transport: &rewriteTransport{target: target, base: http.DefaultTransport},
		Timeout:   5 * time.Second,
	}
	c, err := NewClient("https://api.openai.com", "sk-test", WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func cacheKeyOf(t *testing.T, bodies *[]recordedBody, i int) (string, bool) {
	t.Helper()
	if i >= len(*bodies) {
		t.Fatalf("only %d bodies recorded, want index %d", len(*bodies), i)
	}
	v, ok := (*bodies)[i].raw["prompt_cache_key"]
	if !ok {
		return "", false
	}
	if string(v) == "null" {
		return "null", true
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		t.Fatalf("prompt_cache_key is not a string: %s", v)
	}
	return s, true
}

func TestOpenAIClientAutoCacheKeyWire(t *testing.T) {
	ctx := context.Background()
	usage := `{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,"prompt_tokens_details":{"cached_tokens":800}}`
	mock, bodies := recordMock(t, usage)
	client := openAIClientViaMock(t, mock)

	// Auto key added for OpenAI's own API with a caller + prefix.
	req := sysUserReq()
	req.CallerID = "gk-alice"
	resp, err := client.Chat(ctx, req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	key, present := cacheKeyOf(t, bodies, 0)
	if !present {
		t.Fatal("auto prompt_cache_key missing from the upstream body")
	}
	if !strings.HasPrefix(key, "codvian-") {
		t.Fatalf("key %q lacks the codvian- prefix", key)
	}
	// Provider-reported cached tokens are parsed from the usage block.
	if resp.Usage.CachedTokens != 800 || resp.Usage.PromptTokens != 1000 {
		t.Fatalf("usage = %+v, want prompt 1000 cached 800", resp.Usage)
	}
	// Same caller + same prefix on a different user turn -> the
	// SAME key (user messages excluded), generated per attempt.
	req2 := sysUserReq()
	req2.Messages[1].Content = "Something else entirely."
	req2.CallerID = "gk-alice"
	if _, err := client.Chat(ctx, req2); err != nil {
		t.Fatalf("Chat 2: %v", err)
	}
	key2, present := cacheKeyOf(t, bodies, 1)
	if !present || key2 != key {
		t.Fatalf("second attempt key = %q (present %v), want %q", key2, present, key)
	}
	// Different caller -> different key.
	req3 := sysUserReq()
	req3.CallerID = "gk-bob"
	if _, err := client.Chat(ctx, req3); err != nil {
		t.Fatalf("Chat 3: %v", err)
	}
	key3, present := cacheKeyOf(t, bodies, 2)
	if !present || key3 == key {
		t.Fatalf("different caller key = %q, want a different key than %q", key3, key)
	}
	// No caller -> no key.
	req4 := sysUserReq()
	if _, err := client.Chat(ctx, req4); err != nil {
		t.Fatalf("Chat 4: %v", err)
	}
	if _, present := cacheKeyOf(t, bodies, 3); present {
		t.Fatal("anonymous request must send no prompt_cache_key")
	}
	// No prefix and no tools -> no key even with a caller.
	req5 := ChatRequest{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}, CallerID: "gk-alice"}
	if _, err := client.Chat(ctx, req5); err != nil {
		t.Fatalf("Chat 5: %v", err)
	}
	if _, present := cacheKeyOf(t, bodies, 4); present {
		t.Fatal("bare request must send no prompt_cache_key")
	}
}

func TestOpenAIClientSuppliedCacheKeyNeverReplaced(t *testing.T) {
	ctx := context.Background()
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`
	mock, bodies := recordMock(t, usage)
	client := openAIClientViaMock(t, mock)

	// A client-supplied key is forwarded verbatim.
	raw := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"hi"}],"prompt_cache_key":"my-own-key"}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.CallerID = "gk-alice"
	if _, err := client.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	key, present := cacheKeyOf(t, bodies, 0)
	if !present || key != "my-own-key" {
		t.Fatalf("client key = %q (present %v), want %q", key, present, "my-own-key")
	}
	// An explicit null is preserved as null — never replaced by a
	// generated key.
	rawNull := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"hi"}],"prompt_cache_key":null}`
	var reqNull ChatRequest
	if err := json.Unmarshal([]byte(rawNull), &reqNull); err != nil {
		t.Fatalf("decode null: %v", err)
	}
	if !reqNull.PromptCacheKeySet || reqNull.PromptCacheKey != nil {
		t.Fatalf("null key decoded as set=%v key=%v, want set=true key=nil", reqNull.PromptCacheKeySet, reqNull.PromptCacheKey)
	}
	reqNull.CallerID = "gk-alice"
	if _, err := client.Chat(ctx, reqNull); err != nil {
		t.Fatalf("Chat null: %v", err)
	}
	keyNull, present := cacheKeyOf(t, bodies, 1)
	if !present || keyNull != "null" {
		t.Fatalf("explicit null became %q (present %v), want JSON null", keyNull, present)
	}
	// A non-string prompt_cache_key is a client error, not a
	// silent drop.
	rawBad := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":42}`
	var reqBad ChatRequest
	if err := json.Unmarshal([]byte(rawBad), &reqBad); err == nil {
		t.Fatal("numeric prompt_cache_key must fail to decode")
	}
}

func TestNonOpenAIEndpointGetsNoGeneratedKey(t *testing.T) {
	ctx := context.Background()
	usage := `{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}`
	// A plain mock stands in for any third-party OpenAI-compatible
	// endpoint: the client talks to it directly, so its host is
	// not api.openai.com and no key may be generated.
	mock, bodies := recordMock(t, usage)
	c, err := NewClient(mock.URL, "third-party-key", WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	req := sysUserReq()
	req.CallerID = "gk-alice"
	if _, err := c.Chat(ctx, req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if _, present := cacheKeyOf(t, bodies, 0); present {
		t.Fatal("non-OpenAI endpoint must receive no generated prompt_cache_key")
	}
	// ... but a client-supplied key IS still forwarded there: the
	// "never replaced" rule is about the client's own key, and
	// forwarding what the client asked for is not generating one.
	raw := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"hi"}],"prompt_cache_key":"client-key"}`
	var req2 ChatRequest
	if err := json.Unmarshal([]byte(raw), &req2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req2.CallerID = "gk-alice"
	if _, err := c.Chat(ctx, req2); err != nil {
		t.Fatalf("Chat 2: %v", err)
	}
	key, present := cacheKeyOf(t, bodies, 1)
	if !present || key != "client-key" {
		t.Fatalf("client key on third-party endpoint = %q (present %v), want %q", key, present, "client-key")
	}
}

func TestOpenAIClientAutoCacheKeyStreaming(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		k, ok := raw["prompt_cache_key"]
		if !ok {
			t.Error("streaming request missing auto prompt_cache_key")
		} else {
			var s string
			if err := json.Unmarshal(k, &s); err != nil || !strings.HasPrefix(s, "codvian-") {
				t.Errorf("streaming prompt_cache_key = %s, want codvian- prefix", k)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"hel\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":2,\"total_tokens\":502,\"prompt_tokens_details\":{\"cached_tokens\":400}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	client := openAIClientViaMock(t, srv)
	cs, ok := client.(ChunkStreamer)
	if !ok {
		t.Fatal("openAI client does not implement ChunkStreamer")
	}
	req := sysUserReq()
	req.CallerID = "gk-alice"
	var deltas []string
	resp, err := cs.ChatStreamChunks(ctx, req, func(chunk StreamChunk) error {
		deltas = append(deltas, chunk.Content)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamChunks: %v", err)
	}
	if resp.Content != "hello" {
		t.Fatalf("reassembled content = %q, want %q", resp.Content, "hello")
	}
	if resp.Usage.CachedTokens != 400 || resp.Usage.PromptTokens != 500 {
		t.Fatalf("stream usage = %+v, want prompt 500 cached 400", resp.Usage)
	}
}

// ChatRequest must never serialise the router-internal fields: the
// auto key is generated at send time from the wire struct, never
// from a re-encoded ChatRequest — so a marshal of the decoded
// request must contain no prompt_cache_key, no caller ID, and no
// codvian- key material. (Coalescing keys, cache keys, traces and
// the ledger all marshal the request; this is the assertion that
// none of them can ever carry the key.)
func TestChatRequestMarshalNeverLeaksCacheKey(t *testing.T) {
	raw := `{"model":"gpt-4o","messages":[{"role":"system","content":"You are a coding assistant."},{"role":"user","content":"hi"}],"prompt_cache_key":"my-own-key"}`
	var req ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	req.CallerID = "gk-alice"
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, leak := range []string{"prompt_cache_key", "my-own-key", "gk-alice", "codvian-", "CallerID"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("marshalled ChatRequest leaks %q: %s", leak, b)
		}
	}
	// The decoded client key is still available to the wire layer.
	if req.PromptCacheKey == nil || *req.PromptCacheKey != "my-own-key" || !req.PromptCacheKeySet {
		t.Fatalf("decoded client key = %v set=%v, want my-own-key/set", req.PromptCacheKey, req.PromptCacheKeySet)
	}
}
