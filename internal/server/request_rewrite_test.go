package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// rewriteCaptureEngine records the last ChatRequest the engine
// received, so a test can prove what the provider actually saw —
// verbatim secrets, or the redacted form after a request rewrite.
// It embeds *fakeEngine, so it satisfies Engine whatever the
// interface carries beyond Chat/ChatStream.
type rewriteCaptureEngine struct {
	*fakeEngine
	mu      sync.Mutex
	lastReq provider.ChatRequest
	calls   int
}

func (e *rewriteCaptureEngine) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.lastReq = req
	e.calls++
	e.mu.Unlock()
	return e.fakeEngine.Chat(ctx, req)
}

func (e *rewriteCaptureEngine) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	e.mu.Lock()
	e.lastReq = req
	e.calls++
	e.mu.Unlock()
	return e.fakeEngine.ChatStream(ctx, req, onDelta)
}

func (e *rewriteCaptureEngine) lastRequest() provider.ChatRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastReq
}

func (e *rewriteCaptureEngine) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func rewriteCaptureContent(t *testing.T, eng *rewriteCaptureEngine) string {
	t.Helper()
	req := eng.lastRequest()
	if len(req.Messages) == 0 {
		t.Fatal("engine received a request with no messages")
	}
	return req.Messages[0].Content
}

// TestRequestRewriteRedactsBeforeProvider proves the B3-20 request
// rewrite (parity tracker:783): with WithRequestRewrite on, a chat
// request carrying PII + a secret reaches the engine with both
// stripped — the provider never sees the raw values. Blocking stays
// off (the default), so the request flows with a "flagged" header
// instead of being refused.
func TestRequestRewriteRedactsBeforeProvider(t *testing.T) {
	eng := &rewriteCaptureEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}}}
	srv := newTestServer(t, eng, WithRequestRewrite(true))
	email := "raunak@example.com"
	apiKey := "sk-proj-abcdefghijklmnop12345678"
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "Contact me at "+email+" — my key is "+apiKey, false), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if eng.callCount() != 1 {
		t.Fatalf("engine Chat calls = %d, want 1", eng.callCount())
	}
	if got := rec.Header().Get("X-Codvian-Guardrail"); got != "flagged" {
		t.Fatalf("X-Codvian-Guardrail = %q, want flagged", got)
	}
	got := rewriteCaptureContent(t, eng)
	if strings.Contains(got, email) {
		t.Fatalf("engine received the raw email address: %q", got)
	}
	if strings.Contains(got, apiKey) {
		t.Fatalf("engine received the raw API key: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:") {
		t.Fatalf("engine received no redaction placeholder: %q", got)
	}
	if !strings.Contains(got, "Contact me at") {
		t.Fatalf("rewrite removed more than the matches: %q", got)
	}
}

// TestRequestRewriteOffByDefault is the control for the rewrite:
// without WithRequestRewrite the same request reaches the engine
// verbatim — the rewrite is opt-in, off by default, and the strip
// in TestRequestRewriteRedactsBeforeProvider is its doing, not the
// check's.
func TestRequestRewriteOffByDefault(t *testing.T) {
	eng := &rewriteCaptureEngine{fakeEngine: &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}}}
	srv := newTestServer(t, eng)
	email := "raunak@example.com"
	apiKey := "sk-proj-abcdefghijklmnop12345678"
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "Contact me at "+email+" — my key is "+apiKey, false), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	got := rewriteCaptureContent(t, eng)
	if !strings.Contains(got, email) || !strings.Contains(got, apiKey) {
		t.Fatalf("rewrite off, but engine did not receive the verbatim request: %q", got)
	}
}

// TestRewriteRequestRedactsAllSurfaces proves rewriteRequest covers
// every text surface requestText scans — message content, text
// parts, and tool-call arguments — while leaving non-text parts
// and clean messages untouched, and keeping a redacted tool-call
// argument valid JSON.
func TestRewriteRequestRedactsAllSurfaces(t *testing.T) {
	email := "raunak@example.com"
	apiKey := "sk-proj-abcdefghijklmnop12345678"
	req := provider.ChatRequest{
		Messages: []provider.Message{
			{
				Role:    "user",
				Content: "Contact me at " + email,
				Parts: []provider.ContentPart{
					{Type: "text", Text: "my key is " + apiKey},
					{Type: "image_url", Text: "https://example.com/x.png"},
				},
				ToolCalls: []provider.ToolCall{
					{ID: "call_1", Type: "function", Function: provider.ToolCallFunction{
						Name:      "lookup",
						Arguments: `{"email":"` + email + `"}`,
					}},
				},
			},
			{Role: "user", Content: "clean message, nothing to strip"},
		},
	}
	rewriteRequest(&req)

	msg := req.Messages[0]
	if strings.Contains(msg.Content, email) {
		t.Fatalf("content not redacted: %q", msg.Content)
	}
	if strings.Contains(msg.Parts[0].Text, apiKey) {
		t.Fatalf("text part not redacted: %q", msg.Parts[0].Text)
	}
	if msg.Parts[1].Text != "https://example.com/x.png" {
		t.Fatalf("non-text part changed: %q", msg.Parts[1].Text)
	}
	args := msg.ToolCalls[0].Function.Arguments
	if strings.Contains(args, email) {
		t.Fatalf("tool-call arguments not redacted: %q", args)
	}
	if !json.Valid([]byte(args)) {
		t.Fatalf("redacted tool-call arguments are not valid JSON: %q", args)
	}
	if req.Messages[1].Content != "clean message, nothing to strip" {
		t.Fatalf("clean message changed: %q", req.Messages[1].Content)
	}
}

// TestDetectionsByTypeCardFeed proves the data the dashboard's
// "Detections by type" card (parity tracker:797, B3-25) renders from —
// the /dashboard/guardrails feed's by_pattern per-pattern totals,
// grouped by kind via the types list. A chat carrying an email and
// an API key must land in by_pattern under both labels, with the
// per-kind roll-up at least as large, and the types list must carry
// both patterns enabled.
func TestDetectionsByTypeCardFeed(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}})
	rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
		guardrailChatBody(t, "Contact me at raunak@example.com — my key is sk-proj-abcdefghijklmnop12345678", false), "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("chat status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("feed status = %d, want 200", rec.Code)
	}
	var st guardrails.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode feed: %v", err)
	}
	if st.ByPattern == nil {
		t.Fatal("feed by_pattern is nil — the card would render \"No detections yet\"")
	}
	if st.ByPattern["email"] < 1 {
		t.Fatalf("by_pattern[email] = %d, want >= 1 (%+v)", st.ByPattern["email"], st.ByPattern)
	}
	if st.ByPattern["openai-key"] < 1 {
		t.Fatalf("by_pattern[openai-key] = %d, want >= 1 (%+v)", st.ByPattern["openai-key"], st.ByPattern)
	}
	// The per-kind card is the coarse roll-up of the same
	// detections, so it can never be smaller than a pattern total.
	if st.ByKind[guardrails.KindPII] < st.ByPattern["email"] {
		t.Fatalf("by_kind[pii] = %d < by_pattern[email] = %d", st.ByKind[guardrails.KindPII], st.ByPattern["email"])
	}
	if st.ByKind[guardrails.KindSecret] < st.ByPattern["openai-key"] {
		t.Fatalf("by_kind[secret] = %d < by_pattern[openai-key] = %d", st.ByKind[guardrails.KindSecret], st.ByPattern["openai-key"])
	}
	types := map[string]guardrails.TypeState{}
	for _, ts := range st.Types {
		types[ts.Pattern] = ts
	}
	if types["email"].Kind != "pii" || !types["email"].Enabled {
		t.Fatalf("types[email] = %+v, want kind pii enabled true", types["email"])
	}
	if types["openai-key"].Kind != "secret" || !types["openai-key"].Enabled {
		t.Fatalf("types[openai-key] = %+v, want kind secret enabled true", types["openai-key"])
	}
}

// TestDetectionsByTypeCardDisabledTypeKeepsTotal proves the
// disabled-type half of the card: a disabled type keeps its
// historical total (the card marks it "off") and accrues nothing
// further, while other types stay enabled.
func TestDetectionsByTypeCardDisabledTypeKeepsTotal(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{chatResp: provider.ChatResponse{Content: "ok", Model: "test-model"}})
	chatEmail := func() {
		t.Helper()
		rec := doReq(t, srv, http.MethodPost, "/v1/chat/completions",
			guardrailChatBody(t, "Contact me at raunak@example.com", false), "test-key-123")
		if rec.Code != http.StatusOK {
			t.Fatalf("chat status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
	}
	getFeed := func() guardrails.Stats {
		t.Helper()
		rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("feed status = %d, want 200", rec.Code)
		}
		var st guardrails.Stats
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode feed: %v", err)
		}
		return st
	}
	chatEmail()
	before := getFeed().ByPattern["email"]
	if before < 1 {
		t.Fatalf("by_pattern[email] = %d before disable, want >= 1", before)
	}
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"disabled_types":["email"]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST disabled_types status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	chatEmail()
	after := getFeed()
	if after.ByPattern["email"] != before {
		t.Fatalf("by_pattern[email] moved %d -> %d after disable, want flat (historical total kept)", before, after.ByPattern["email"])
	}
	types := map[string]guardrails.TypeState{}
	for _, ts := range after.Types {
		types[ts.Pattern] = ts
	}
	if types["email"].Enabled {
		t.Fatalf("types[email] still enabled after disable: %+v", types["email"])
	}
	if !types["openai-key"].Enabled {
		t.Fatalf("types[openai-key] disabled, want only email disabled: %+v", types["openai-key"])
	}
}
