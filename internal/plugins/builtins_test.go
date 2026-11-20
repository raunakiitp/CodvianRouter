package plugins

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestHeaderAddPluginAddsHeader(t *testing.T) {
	p, err := NewHeaderAddPlugin("X-Test-Plugin", "yes")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != HeaderAddName {
		t.Fatalf("name = %q", p.Name())
	}
	if len(p.Hooks()) != 1 || p.Hooks()[0] != HookPreRequest {
		t.Fatalf("hooks = %v", p.Hooks())
	}
	req := testReq()
	deny, reason, err := p.(PreRequestPlugin).PreRequest(context.Background(), req)
	if deny || reason != "" || err != nil {
		t.Fatalf("deny=%v reason=%q err=%v", deny, reason, err)
	}
	if req.Headers["X-Test-Plugin"] != "yes" {
		t.Fatalf("headers = %v", req.Headers)
	}
	// A nil header map is created, not panicked on.
	req2 := &Request{Chat: &provider.ChatRequest{Model: "m"}}
	if _, _, err := p.(PreRequestPlugin).PreRequest(context.Background(), req2); err != nil {
		t.Fatal(err)
	}
	if req2.Headers["X-Test-Plugin"] != "yes" {
		t.Fatalf("headers = %v", req2.Headers)
	}
}

func TestHeaderAddPluginValidation(t *testing.T) {
	if _, err := NewHeaderAddPlugin("", "v"); err == nil {
		t.Fatal("empty header name accepted")
	}
	if _, err := NewHeaderAddPlugin("X-H", ""); err == nil {
		t.Fatal("empty header value accepted")
	}
}

func TestResponseLogPluginRecordsShapeNotContent(t *testing.T) {
	p := NewResponseLogPlugin().(PostResponsePlugin)
	req := testReq()
	resp := &provider.ChatResponse{Model: "gpt-x", Content: "SECRET-ANSWER-DO-NOT-STORE", FinishReason: "stop", Usage: provider.Usage{PromptTokens: 10, CompletionTokens: 5}}
	if err := p.PostResponse(context.Background(), req, resp); err != nil {
		t.Fatal(err)
	}
	// Nil response must not panic a logging hook.
	if err := p.PostResponse(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	recent := p.(RecentResponder).RecentResponses()
	if len(recent) != 1 {
		t.Fatalf("recent = %d, want 1 (nil response ignored)", len(recent))
	}
	ev := recent[0]
	if ev.Model != "gpt-x" || ev.FinishReason != "stop" || ev.ContentChars != len("SECRET-ANSWER-DO-NOT-STORE") {
		t.Fatalf("event = %+v", ev)
	}
	if ev.PromptTokens != 10 || ev.CompletionTokens != 5 {
		t.Fatalf("tokens = %+v", ev)
	}
	// The response model falls back to the request model when empty.
	resp2 := &provider.ChatResponse{Content: "x"}
	_ = p.PostResponse(context.Background(), req, resp2)
	recent = p.(RecentResponder).RecentResponses()
	if recent[1].Model != "m" || recent[1].FinishReason != "stop" {
		t.Fatalf("fallback event = %+v", recent[1])
	}
}

func TestResponseLogPluginRingBounded(t *testing.T) {
	p := NewResponseLogPlugin().(PostResponsePlugin)
	for i := 0; i < maxRecentResponses+5; i++ {
		_ = p.PostResponse(context.Background(), testReq(), &provider.ChatResponse{Model: "m", Content: "x"})
	}
	if got := len(p.(RecentResponder).RecentResponses()); got != maxRecentResponses {
		t.Fatalf("recent = %d, want %d (bounded ring)", got, maxRecentResponses)
	}
}

func TestBuiltinsRunEndToEndInRegistry(t *testing.T) {
	r := New()
	if err := LoadBuiltins(r, []string{HeaderAddName, ResponseLogName}); err != nil {
		t.Fatal(err)
	}
	req := testReq()
	out := r.PreRequest(context.Background(), req)
	if out.Denied || out.Err != nil || out.Panic != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if req.Headers["X-Codvian-Plugin"] != "header-add" {
		t.Fatalf("headers = %v, want plugin header set", req.Headers)
	}
	failures := r.PostResponse(context.Background(), req, &provider.ChatResponse{Model: "m", Content: "hi"})
	if len(failures) != 0 {
		t.Fatalf("failures = %v", failures)
	}
	snap := r.Snapshot()
	if snap[0].Stats[HookPreRequest].Calls != 1 || snap[1].Stats[HookPostResponse].Calls != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap[1].Recent) != 1 || snap[1].Recent[0].ContentChars != 2 {
		t.Fatalf("recent = %+v", snap[1].Recent)
	}
}
