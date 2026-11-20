package degrade

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// M6.6b — resume paths serve exact hits (PARITY :738, B2-32).
//
// ChatStreamFrom consults exactLookup BEFORE injectReasoning and
// before any upstream call, so a client reconnecting mid-stream
// replays the cached answer trimmed by skipChars without touching
// the provider. Until this test existed, that branch was unpinned:
// the 13 resume tests all run with serving disabled, so removing
// the exactLookup call changed nothing they assert (V-B232,
// :738 STAYS PARTIAL — resume exact-serving untested).
func TestM66bChatStreamFromServesExactHitTrimmed(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{}
	se, _ := wrapTest(f, serveCachePolicy())

	// Seed the exact cache with a long answer via Chat.
	const answer = "hello world, this is the answer"
	f.chatResp = &provider.ChatResponse{Content: answer, Model: "test-model"}
	if _, err := se.Chat(ctx, testReq()); err != nil {
		t.Fatalf("seed Chat: %v", err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("seed chatCalls = %d, want 1", f.chatCalls)
	}

	// Arm the fallback path so the toggle (exactLookup removed)
	// produces a live upstream call instead of a nil response:
	// the resume must NOT reach it.
	f.streamResp = &provider.ChatResponse{Content: answer, Model: "test-model"}
	f.streamDeltas = []string{answer}

	// The client already has the first 6 runes ("hello ").
	var deltas []string
	resp, err := se.ChatStreamFrom(ctx, testReq(), 6, func(d string) error {
		deltas = append(deltas, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStreamFrom: %v", err)
	}
	if resp == nil || resp.Content != answer {
		t.Fatalf("ChatStreamFrom content = %v, want full cached answer %q", resp, answer)
	}
	if got := strings.Join(deltas, ""); got != "world, this is the answer" {
		t.Fatalf("delivered deltas = %q, want trimmed suffix %q (skipChars=6)", got, "world, this is the answer")
	}
	if f.chatCalls != 1 || f.streamCalls != 0 {
		t.Fatalf("upstream calls chat=%d stream=%d, want 1/0: the resume must serve the exact hit, not the provider", f.chatCalls, f.streamCalls)
	}
	if kind, _, ok := se.LastCacheHit(); !ok || kind != "exact" {
		t.Fatalf("LastCacheHit = %q,%v, want exact,true", kind, ok)
	}
}
