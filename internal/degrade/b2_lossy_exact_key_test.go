// B2-26 (LossyExactHit) pin: the exact-cache key is the
// PRE-COMPRESSION request (raw messages + compression params),
// not the messages as sent. The CCR pre-pass is stateful —
// request 1 Puts content shas, request 2's identical content
// gets markered — and the lossy trim counts post-CCR token
// lengths, so two identical logical requests reach the engine
// with different messages. Keying on the messages as sent made
// the repeat miss (TestChatCompletionsLossyExactHitStillWorks).
// These tests pin the mechanism at the engine level: same
// snapshot + different live messages => same key and a cache
// hit; different compression params => a different key and a
// miss; no snapshot at all => the old messages-as-sent keying.
package degrade

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// b2LossyRawMsgs is a 4-turn history like the lossy HTTP test's:
// two long assistant turns whose content the CCR pre-pass markers
// on the second identical request.
func b2LossyRawMsgs() []provider.Message {
	long := "The expedition launched in the morning and travelled for three days through the dark sky. "
	return []provider.Message{
		{Role: "user", Content: "Tell me a long story about the first expedition to the moon, in great detail. " + long},
		{Role: "assistant", Content: "Once upon a time, a long time ago, people dreamed of the moon... " + long},
		{Role: "user", Content: "Continue the story with the landing itself, every detail you know. " + long},
		{Role: "assistant", Content: "The landing was a careful descent through the thin lunar sky... " + long},
	}
}

// b2LossyMarkeredMsgs is b2LossyRawMsgs as the engine sees it on
// the second identical request: the assistant turns replaced by
// CCR markers, as ApplyCCR does once the store is warm.
func b2LossyMarkeredMsgs() []provider.Message {
	msgs := b2LossyRawMsgs()
	msgs[1].Content = "[ccr:ref sha=0123456789abcdef]"
	msgs[3].Content = "[ccr:ref sha=fedcba9876543210]"
	return msgs
}

func TestB2LossyExactKeyUsesPreCompressionSnapshot(t *testing.T) {
	raw := b2LossyRawMsgs()
	comp := &provider.CompressionRequest{LossyMaxPromptTokens: 40}
	base := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: raw, Compression: comp}
	snap := base.SnapshotPreCompression()

	se, _, _, _ := wrapSemanticTest(&fakeEngine{chatResp: m611Resp("x", "stop")}, m611Policy())

	// Same snapshot, different live messages (raw vs markered):
	// the key must be identical.
	reqRaw := base
	reqRaw.PreCompression = snap
	reqMarked := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: b2LossyMarkeredMsgs(), Compression: comp}
	reqMarked.PreCompression = snap
	kRaw := m611ScopedKey(t, se, reqRaw)
	kMarked := m611ScopedKey(t, se, reqMarked)
	if kRaw != kMarked {
		t.Fatalf("scopedKey raw vs markered = %q vs %q, want equal (same pre-compression snapshot)", kRaw, kMarked)
	}

	// Same snapshot messages, different compression params:
	// the key must differ.
	comp100 := &provider.CompressionRequest{LossyMaxPromptTokens: 100}
	req100 := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: raw, Compression: comp100}
	req100.PreCompression = req100.SnapshotPreCompression()
	if k100 := m611ScopedKey(t, se, req100); k100 == kRaw {
		t.Fatalf("scopedKey lossy-100 = %q, want different from lossy-40 %q", k100, kRaw)
	}

	// No snapshot at all: the old messages-as-sent keying —
	// markered messages must key differently from raw ones.
	reqNoSnapRaw := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: raw, Compression: comp}
	reqNoSnapMarked := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: b2LossyMarkeredMsgs(), Compression: comp}
	if m611ScopedKey(t, se, reqNoSnapRaw) == m611ScopedKey(t, se, reqNoSnapMarked) {
		t.Fatalf("scopedKey without snapshot: raw and markered keyed equal, want different (no snapshot, messages as sent)")
	}
}

func TestB2LossyExactHitThroughChat(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: m611Resp("TRIMMED", "stop")}
	se, _, _, _ := wrapSemanticTest(f, m611Policy())

	raw := b2LossyRawMsgs()
	comp := &provider.CompressionRequest{LossyMaxPromptTokens: 40}
	snap := (&provider.ChatRequest{Messages: raw, Compression: comp}).SnapshotPreCompression()

	// Request 1: raw messages, snapshot taken before any
	// pre-pass mutation — stores under the pre-compression key.
	req1 := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: raw, Compression: comp}
	req1.PreCompression = snap
	if resp, err := se.Chat(ctx, req1); err != nil || resp == nil || resp.Content != "TRIMMED" {
		t.Fatalf("request 1 Chat = (%+v, %v), want TRIMMED from provider", resp, err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("request 1 provider calls = %d, want 1", f.chatCalls)
	}

	// Request 2: identical logical request, but the engine sees
	// markered messages (CCR store warm) — the snapshot is the
	// same raw one, so this must hit the exact entry with no
	// second provider call.
	req2 := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: b2LossyMarkeredMsgs(), Compression: comp}
	req2.PreCompression = snap
	if resp, err := se.Chat(ctx, req2); err != nil || resp == nil || resp.Content != "TRIMMED" {
		t.Fatalf("request 2 Chat = (%+v, %v), want TRIMMED from exact cache", resp, err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("provider calls after identical repeat = %d, want 1 (exact hit, no provider call)", f.chatCalls)
	}

	// Control: the same markered messages WITHOUT a snapshot
	// must miss — the snapshot is what makes the hit, not the
	// messages themselves.
	req3 := provider.ChatRequest{CallerID: "t", Model: "test-model", Messages: b2LossyMarkeredMsgs(), Compression: comp}
	if resp, err := se.Chat(ctx, req3); err != nil || resp == nil || resp.Content != "TRIMMED" {
		t.Fatalf("request 3 (no snapshot) Chat = (%+v, %v), want TRIMMED from provider", resp, err)
	}
	if f.chatCalls != 2 {
		t.Fatalf("provider calls after no-snapshot repeat = %d, want 2 (miss, provider called)", f.chatCalls)
	}
}
