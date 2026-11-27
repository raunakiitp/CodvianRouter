package webexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// executorFor builds an executor wired to a mock web surface for one
// catalog ID, with the login state saved first. This is the full
// production path minus the live origin: state storage → executor →
// HTTP session replay against the surface.
func executorFor(t *testing.T, id string, surface *mockWebSurface, token string, opts ...Option) Executor {
	t.Helper()
	store := NewSessionStore(nil)
	if err := store.Save(id, surface.sessionStateFor(token)); err != nil {
		t.Fatalf("%s: save session: %v", id, err)
	}
	base := []Option{WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client())}
	ex, err := New(id, store, append(base, opts...)...)
	if err != nil {
		t.Fatalf("%s: new executor: %v", id, err)
	}
	return ex
}

// TestWebCookieExecutorPerID is THE per-ID proof: for every one of the
// 37 WebCookie catalog IDs, the stored login state loads and a chat
// request executes against the mock web surface, with the session
// cookie replayed verbatim and the prompt delivered. These tests never
// skip — if the executor path is disabled or broken, they FAIL.
func TestWebCookieExecutorPerID(t *testing.T) {
	for _, id := range CatalogIDsWithWebExec() {
		t.Run(id, func(t *testing.T) {
			surface := newMockWebSurface()
			defer surface.close()

			// 1. Login state loads: the stored session must round-trip
			//    out of the store with its cookie intact.
			store := NewSessionStore(nil)
			if err := store.Save(id, surface.sessionStateFor(mockValidToken)); err != nil {
				t.Fatalf("save session: %v", err)
			}
			loaded, err := store.Load(id)
			if err != nil {
				t.Fatalf("login state does not load: %v", err)
			}
			if len(loaded.Cookies) == 0 || loaded.Cookies[0].Value != mockValidToken {
				t.Fatalf("loaded state lost the session cookie: %+v", loaded.Cookies)
			}

			// 2. A chat request executes against the mock web surface.
			ex := executorFor(t, id, surface, mockValidToken)
			res, err := ex.Execute(context.Background(), ExecuteRequest{
				Prompt:        fmt.Sprintf("ping %s", id),
				Model:         id + "-model",
				TemporaryChat: true,
			})
			if err != nil {
				t.Fatalf("chat request failed: %v", err)
			}
			want := "mock reply to: ping " + id
			if surfaces[id].TemporaryChat {
				want += " [temporary]"
			}
			if res.Content != want {
				t.Fatalf("content = %q, want %q", res.Content, want)
			}
			if res.StatusCode != 200 {
				t.Fatalf("status = %d, want 200", res.StatusCode)
			}
			if res.Surface != id {
				t.Fatalf("surface = %q, want %q", res.Surface, id)
			}

			// 3. The executor actually drove the surface: the mock saw
			//    the replayed session cookie and the prompt.
			saw := surface.lastHit()
			if !strings.Contains(saw.CookieHeader, mockSessionCookie+"="+mockValidToken) {
				t.Fatalf("executor did not replay the session cookie (got %q)", saw.CookieHeader)
			}
			if saw.Prompt != "ping "+id {
				t.Fatalf("surface saw prompt %q", saw.Prompt)
			}
			if saw.Model != id+"-model" {
				t.Fatalf("surface saw model %q", saw.Model)
			}
			if surface.hitCount() != 1 {
				t.Fatalf("surface hit %d times, want 1", surface.hitCount())
			}

			// 4. Temporary chat is plumbed exactly where the surface
			//    supports it — never silently elsewhere.
			if surfaces[id].TemporaryChat {
				if !saw.TemporarySet || !saw.TemporaryFlag {
					t.Fatalf("surface supports temporary chat but the executor did not send temporary=true")
				}
				if !res.TemporaryChat {
					t.Fatalf("result must mark the execution temporary")
				}
			} else if saw.TemporarySet {
				t.Fatalf("surface has no temporary-chat support but the executor sent a temporary flag")
			}
		})
	}
}

// TestWebCookieExecutorPerIDNoSession proves the no-session refusal
// per ID: an executor with nothing stored must answer ErrNoSession
// (401 semantics at the adapter), never a fabricated answer.
func TestWebCookieExecutorPerIDNoSession(t *testing.T) {
	for _, id := range CatalogIDsWithWebExec() {
		t.Run(id, func(t *testing.T) {
			surface := newMockWebSurface()
			defer surface.close()
			store := NewSessionStore(nil)
			ex, err := New(id, store, WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
			if err != nil {
				t.Fatalf("new executor: %v", err)
			}
			_, err = ex.Execute(context.Background(), ExecuteRequest{Prompt: "hi"})
			if !errors.Is(err, ErrNoSession) {
				t.Fatalf("want ErrNoSession, got: %v", err)
			}
			if surface.hitCount() != 0 {
				t.Fatalf("executor hit the surface without a session (%d hits)", surface.hitCount())
			}
		})
	}
}

// TestWebCookieExecutorPerIDTemporaryChatDefault covers surfaces
// WITHOUT temporary-chat support: a TemporaryChat request must degrade
// to a normal (history-carrying) execution, not error.
func TestWebCookieExecutorPerIDTemporaryChatDefault(t *testing.T) {
	for _, id := range CatalogIDsWithWebExec() {
		if surfaces[id].TemporaryChat {
			continue
		}
		t.Run(id, func(t *testing.T) {
			surface := newMockWebSurface()
			defer surface.close()
			ex := executorFor(t, id, surface, mockValidToken)
			res, err := ex.Execute(context.Background(), ExecuteRequest{Prompt: "plain", TemporaryChat: true})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			if res.TemporaryChat {
				t.Fatalf("%s: result claims temporary chat though the surface does not support it", id)
			}
		})
	}
}

// Test401OnStaleSessionState is the 401-on-stale proof: a stored state
// the provider no longer accepts produces ErrStaleSession at the
// executor and a 401 UpstreamError through the provider.Client
// adapter — the router fails over instead of retrying a dead session.
func Test401OnStaleSessionState(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()

	// A stale token: stored, but not the one the mock accepts.
	ex := executorFor(t, "claude-web", surface, "expired-token-value")
	_, err := ex.Execute(context.Background(), ExecuteRequest{Prompt: "hello"})
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("want ErrStaleSession, got: %v", err)
	}

	// Through the provider.Client adapter: upstream 401.
	client, err := NewWebExecClient("claude-web", sessionStoreFor(t, "claude-web", surface, "expired-token-value"), WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	_, cerr := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "claude-web",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var ue *provider.UpstreamError
	if !errors.As(cerr, &ue) || ue.StatusCode != 401 {
		t.Fatalf("adapter must map stale state to UpstreamError 401, got: %v (%T)", cerr, cerr)
	}
}

// Test401OnStaleCapturedAge proves a too-old captured state is refused
// before any network call: the store classifies it stale, the executor
// never reaches the surface.
func Test401OnStaleCapturedAge(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()
	store := NewSessionStore(nil)
	state := surface.sessionStateFor(mockValidToken)
	state.CapturedAt = time.Now().Add(-40 * 24 * time.Hour)
	if err := store.Save("chatgpt-web", state); err != nil {
		t.Fatalf("save: %v", err)
	}
	ex, err := New("chatgpt-web", store, WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
	if err != nil {
		t.Fatalf("new executor: %v", err)
	}
	_, err = ex.Execute(context.Background(), ExecuteRequest{Prompt: "hi"})
	if !errors.Is(err, ErrStaleSession) {
		t.Fatalf("want ErrStaleSession for an old capture, got: %v", err)
	}
	if surface.hitCount() != 0 {
		t.Fatalf("executor hit the surface with a known-stale state")
	}
}

// Test429QuotaPassthrough is the 429 proof: the provider's quota
// answer passes through as provider.RateLimitError with the
// Retry-After preserved, so account fallback / pacing sees the real
// upstream signal (and no retry storm).
func Test429QuotaPassthrough(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()

	// Executor level: ErrQuotaExceeded.
	ex := executorFor(t, "grok-web", surface, mockQuotaToken)
	_, err := ex.Execute(context.Background(), ExecuteRequest{Prompt: "hello"})
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("want ErrQuotaExceeded, got: %v", err)
	}

	// Adapter level: RateLimitError with Retry-After 7s.
	client, err := NewWebExecClient("grok-web", sessionStoreFor(t, "grok-web", surface, mockQuotaToken), WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	_, cerr := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "grok-web",
		Messages: []provider.Message{{Role: "user", Content: "hello"}},
	})
	var rle *provider.RateLimitError
	if !errors.As(cerr, &rle) {
		t.Fatalf("adapter must map quota to RateLimitError, got: %v (%T)", cerr, cerr)
	}
	if !rle.RetryAfterPresent || rle.RetryAfter != 7*time.Second {
		t.Fatalf("Retry-After must pass through as 7s, got present=%v ra=%v", rle.RetryAfterPresent, rle.RetryAfter)
	}
}

// TestExecutorPathDisabledFails is the "every test must fail when the
// executor path is disabled" gate, exercised directly: a disabled
// executor refuses EVERY execution with ErrExecutorDisabled, hits
// nothing, and the adapter maps it to a plain (non-401/429) error —
// so any test that would pass while disabled fails loudly.
func TestExecutorPathDisabledFails(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()
	ex := executorFor(t, "chatgpt-web", surface, mockValidToken, WithEnabled(false))
	_, err := ex.Execute(context.Background(), ExecuteRequest{Prompt: "hi"})
	if !errors.Is(err, ErrExecutorDisabled) {
		t.Fatalf("disabled executor must refuse with ErrExecutorDisabled, got: %v", err)
	}
	if surface.hitCount() != 0 {
		t.Fatalf("disabled executor hit the surface")
	}

	client, err := NewWebExecClient("chatgpt-web", sessionStoreFor(t, "chatgpt-web", surface, mockValidToken), WithEnabled(false))
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	_, cerr := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "chatgpt-web",
		Messages: []provider.Message{{Role: "user", Content: "hi"}},
	})
	if !errors.Is(cerr, ErrExecutorDisabled) {
		t.Fatalf("adapter must pass the disabled refusal through: %v", cerr)
	}
	var ue *provider.UpstreamError
	var rle *provider.RateLimitError
	if errors.As(cerr, &ue) || errors.As(cerr, &rle) {
		t.Fatalf("disabled refusal must not be classified as 401/429: %v", cerr)
	}
}

// TestAdapterChatAndStream covers the provider.Client surface of the
// adapter against the mock web UI.
func TestAdapterChatAndStream(t *testing.T) {
	surface := newMockWebSurface()
	defer surface.close()
	client, err := NewWebExecClient("poe-web", sessionStoreFor(t, "poe-web", surface, mockValidToken), WithBaseURL(surface.URL()), WithMockContract(), WithHTTPClient(surface.server.Client()))
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	resp, err := client.Chat(context.Background(), provider.ChatRequest{
		Model:    "poe-web",
		Messages: []provider.Message{{Role: "user", Content: "adapter check"}},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "mock reply to: adapter check" {
		t.Fatalf("content = %q", resp.Content)
	}

	var streamed strings.Builder
	resp2, err := client.ChatStream(context.Background(), provider.ChatRequest{
		Model:    "poe-web",
		Messages: []provider.Message{{Role: "user", Content: "stream check"}},
	}, func(delta string) error { streamed.WriteString(delta); return nil })
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if streamed.String() != resp2.Content {
		t.Fatalf("stream deltas (%q) != content (%q)", streamed.String(), resp2.Content)
	}

	models, err := client.ListModels(context.Background())
	if err != nil || len(models) == 0 {
		t.Fatalf("list models: %v %v", models, err)
	}
	if err := client.TestKey(context.Background()); err != nil {
		t.Fatalf("test key with session: %v", err)
	}

	// No session → TestKey fails honestly.
	empty, err := NewWebExecClient("poe-web", NewSessionStore(nil))
	if err != nil {
		t.Fatalf("adapter: %v", err)
	}
	if err := empty.TestKey(context.Background()); !errors.Is(err, ErrNoSession) {
		t.Fatalf("TestKey without session must fail with ErrNoSession: %v", err)
	}
}

// TestAdapterRefusesNonWebCookieIDs: the adapter only serves the 37
// WebCookie IDs.
func TestAdapterRefusesNonWebCookieIDs(t *testing.T) {
	_, err := NewWebExecClient("openai", NewSessionStore(nil))
	if !errors.Is(err, ErrUnknownSurface) {
		t.Fatalf("non-web-cookie ID must be refused: %v", err)
	}
}

// sessionStoreFor builds a fresh store with one saved session for a
// catalog ID (helper for adapter tests).
func sessionStoreFor(t *testing.T, id string, surface *mockWebSurface, token string) *SessionStore {
	t.Helper()
	store := NewSessionStore(nil)
	if err := store.Save(id, surface.sessionStateFor(token)); err != nil {
		t.Fatalf("%s: save session: %v", id, err)
	}
	return store
}
