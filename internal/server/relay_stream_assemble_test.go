// A13 Stream B (H2/N1): streamed relay responses are assembled and stored.
//
// FAILS-WHEN-OFF: with storeAssembledStreamCache removed (or called with a
// nil assembler), the streamed branch stores nothing and every "the later
// identical request hits the cache" assertion fails.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/cache"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/wire"
)

// sseFor builds an Anthropic SSE stream for a simple text answer.
func sseFor(text string, inTok, outTok int) string {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"role\":\"assistant\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":" + itoa(inTok) + "}}}\n\n")
	b.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	for _, chunk := range splitWords(text) {
		b.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":" + jsonStr(chunk) + "}}\n\n")
	}
	b.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	b.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":" + itoa(outTok) + "}}\n\n")
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func splitWords(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		cur += string(r)
		if r == ' ' {
			out = append(out, cur)
			cur = ""
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// TestStreamAssemblerReconstructsMessage: the assembler rebuilds the exact
// text, model, stop reason and provider-reported usage from the event stream.
func TestStreamAssemblerReconstructsMessage(t *testing.T) {
	a := newStreamAssembler()
	a.Write([]byte(sseFor("hello there world", 11, 7)))
	if !a.Complete() {
		t.Fatalf("assembler reported incomplete for a well-formed stream")
	}
	resp := a.Response()
	if resp.Model != "claude-test" {
		t.Errorf("model = %q, want claude-test", resp.Model)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text block", resp.Content)
	}
	if resp.Content[0].Text != "hello there world" {
		t.Errorf("text = %q, want %q (deltas concatenated in order)", resp.Content[0].Text, "hello there world")
	}
	if resp.StopReason == nil || *resp.StopReason != "end_turn" {
		t.Errorf("stop reason = %v, want end_turn", resp.StopReason)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v, want input 11 / output 7 from the provider's own events", resp.Usage)
	}
}

// TestStreamAssemblerTruncatedIsIncomplete: a stream with no message_stop
// (the connection dropped, or a lifetime cap cut it) must never be stored.
func TestStreamAssemblerTruncatedIsIncomplete(t *testing.T) {
	full := sseFor("partial answer here", 5, 3)
	cut := full[:strings.Index(full, "event: message_stop")]
	a := newStreamAssembler()
	a.Write([]byte(cut))
	if a.Complete() {
		t.Fatal("truncated stream reported complete; a partial answer must never be cacheable")
	}
}

// TestStreamAssemblerToolInputDetected: a tool_use stream is recognised so
// the store's tool-call refusal can apply to streamed responses too.
func TestStreamAssemblerToolInputDetected(t *testing.T) {
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"get_weather\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Pune\\\"}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	a := newStreamAssembler()
	a.Write([]byte(stream))
	if !a.Complete() {
		t.Fatal("tool stream reported incomplete")
	}
	if !a.HasToolInput() {
		t.Error("HasToolInput() = false for an input_json_delta stream")
	}
	resp := a.Response()
	if len(resp.Content) != 1 || resp.Content[0].Type != "tool_use" || resp.Content[0].Name != "get_weather" {
		t.Fatalf("content = %+v, want one tool_use block named get_weather", resp.Content)
	}
	if string(resp.Content[0].Input) != `{"city":"Pune"}` {
		t.Errorf("tool input = %s, want the reassembled JSON", resp.Content[0].Input)
	}
}

// TestStreamAssemblerUnknownEventIsIncomplete: an event type this relay
// cannot represent makes the assembly unusable for caching.
func TestStreamAssemblerUnknownEventIsIncomplete(t *testing.T) {
	stream := sseFor("ok", 1, 1) + "event: some_future_event\ndata: {\"type\":\"some_future_event\"}\n\n"
	a := newStreamAssembler()
	a.Write([]byte(stream))
	if a.Complete() {
		t.Fatal("unknown event type left the assembly 'complete'; it must not be cacheable")
	}
}

// streamStoreServer builds a server with a live exact cache and returns it
// with the cache so a test can assert what was stored.
func streamStoreServer(t *testing.T) (*Server, *cache.ExactCache) {
	t.Helper()
	exact := cache.NewExactCache(64)
	srv := &Server{exactCache: exact}
	return srv, exact
}

// TestStoreAssembledStreamCacheStoresCompletedStream proves the store path
// puts a completed streamed answer into the exact layer under the SAME key
// the non-streaming branch uses, so a later identical request can hit it.
//
// FAILS-WHEN-OFF: with storeAssembledStreamCache not called (or made a
// no-op), the lookup below misses.
func TestStoreAssembledStreamCacheStoresCompletedStream(t *testing.T) {
	srv, exact := streamStoreServer(t)
	req := provider.ChatRequest{Model: "claude-test", CallerID: "tenant-a"}
	asm := newStreamAssembler()
	asm.Write([]byte(sseFor("cached streamed answer", 12, 6)))

	srv.storeAssembledStreamCache(context.Background(), req, asm, true, http.StatusOK)

	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	got, ok := exact.Get(context.Background(), key)
	if !ok {
		t.Fatal("completed streamed response was NOT stored: a later identical request cannot hit the cache")
	}
	var found bool
	for _, block := range []string{got.Content} {
		if strings.Contains(block, "cached streamed answer") {
			found = true
		}
	}
	if !found {
		t.Fatalf("stored content = %q, want the assembled stream text", got.Content)
	}
	if got.Usage.CompletionTokens != 6 {
		t.Errorf("stored completion tokens = %d, want 6 (the provider's own message_delta usage)", got.Usage.CompletionTokens)
	}
}

// TestStoreAssembledStreamCacheRefusesIncomplete: a truncated stream is
// never stored, so the cache can never serve a partial answer.
func TestStoreAssembledStreamCacheRefusesIncomplete(t *testing.T) {
	srv, exact := streamStoreServer(t)
	req := provider.ChatRequest{Model: "claude-test", CallerID: "tenant-a"}
	full := sseFor("partial", 3, 2)
	asm := newStreamAssembler()
	asm.Write([]byte(full[:strings.Index(full, "event: message_stop")]))

	srv.storeAssembledStreamCache(context.Background(), req, asm, true, http.StatusOK)

	key, _ := scopedMessagesKey(req)
	if _, ok := exact.Get(context.Background(), key); ok {
		t.Fatal("truncated stream WAS stored; the cache must never hold a partial answer")
	}
}

// TestStoreAssembledStreamCacheRefusesNonCacheable / non-2xx: the same gates
// as the buffered branch.
func TestStoreAssembledStreamCacheGates(t *testing.T) {
	sse := sseFor("gated answer", 2, 2)
	cases := []struct {
		name      string
		cacheable bool
		status    int
	}{
		{"uncacheable request never stores", false, http.StatusOK},
		{"non-2xx never stores", true, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, exact := streamStoreServer(t)
			req := provider.ChatRequest{Model: "claude-test", CallerID: "tenant-a"}
			asm := newStreamAssembler()
			asm.Write([]byte(sse))
			srv.storeAssembledStreamCache(context.Background(), req, asm, tc.cacheable, tc.status)
			key, _ := scopedMessagesKey(req)
			if _, ok := exact.Get(context.Background(), key); ok {
				t.Fatalf("%s: entry was stored anyway", tc.name)
			}
		})
	}
}

// TestStoreAssembledStreamCacheRefusesToolCalls: a streamed tool-carrying
// answer goes through the same refusal as a buffered one.
func TestStoreAssembledStreamCacheRefusesToolCalls(t *testing.T) {
	srv, exact := streamStoreServer(t)
	req := provider.ChatRequest{Model: "claude-test", CallerID: "tenant-a"}
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"role\":\"assistant\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tu1\",\"name\":\"f\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	asm := newStreamAssembler()
	asm.Write([]byte(stream))
	srv.storeAssembledStreamCache(context.Background(), req, asm, true, http.StatusOK)
	key, _ := scopedMessagesKey(req)
	if _, ok := exact.Get(context.Background(), key); ok {
		t.Fatal("tool-carrying streamed response was stored; cache.ShouldStore's tool-call refusal must apply to streams too")
	}
}

// TestAssemblerHandlesSplitChunks: the assembler must work on arbitrary byte
// boundaries — the relay reads 32 KiB chunks, so an event can be split
// anywhere, including inside a JSON string.
func TestAssemblerHandlesSplitChunks(t *testing.T) {
	full := []byte(sseFor("split across reads", 9, 4))
	a := newStreamAssembler()
	for _, b := range full {
		a.Write([]byte{b})
	}
	if !a.Complete() {
		t.Fatal("byte-at-a-time feed produced an incomplete assembly")
	}
	resp := a.Response()
	if len(resp.Content) == 0 || resp.Content[0].Text != "split across reads" {
		t.Fatalf("content = %+v, want the full text", resp.Content)
	}
}

// keep the wire import honest for the block assertions above
var _ = wire.AnthropicBlock{}
