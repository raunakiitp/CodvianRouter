// M6.11-fix — engine-level proof that the two BLOCKED M6.11
// findings are closed through the real StaleEngine (Chat /
// ChatStream), on paths the M6.11 property tests do not cover:
// streaming (tenant), the anonymous tenant in both directions,
// the temperature-bucket edge, and the re-store laundering (a
// cross-tenant / cross-temperature semantic hit used to be
// re-stored under the requester's exact key, laundering the
// other tenant's / temperature's answer into the exact layer).
package degrade

import (
	"context"
	"errors"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestM611FixCrossTenantParaphraseStream proves BLOCKED-1 on the
// streaming path: tenant B's paraphrase stream with providers
// down errors with zero deltas (no replay of A's answer), while
// tenant A's own paraphrase still replays semantically. It also
// proves no laundering: after B's failed attempt the exact cache
// holds nothing under B's key.
func TestM611FixCrossTenantParaphraseStream(t *testing.T) {
	ctx := context.Background()
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, exact, _, _ := wrapSemanticTest(f, m611Policy())
	if _, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France?", 0)); err != nil {
		t.Fatalf("tenant A store Chat: %v", err)
	}
	// Control: same-tenant paraphrase replays semantically.
	var got []string
	if _, err := se.ChatStream(ctx, m611Req("tenant-a", "test-model", "What is the capital of France today?", 0), func(d string) error { got = append(got, d); return nil }); err != nil {
		t.Fatalf("same-tenant paraphrase ChatStream: %v", err)
	}
	if len(got) != 1 || got[0] != "Paris is the capital of France." {
		t.Fatalf("same-tenant paraphrase deltas = %q, want the Paris answer replayed", got)
	}
	// Property: tenant B's paraphrase errors, delivers nothing.
	m611FailProvider(f)
	var bGot []string
	bReq := m611Req("tenant-b", "test-model", "What is the capital of France today?", 0)
	if _, err := se.ChatStream(ctx, bReq, func(d string) error { bGot = append(bGot, d); return nil }); !errors.Is(err, errUpstreamDown) {
		t.Fatalf("tenant B paraphrase ChatStream err = %v, want the upstream error", err)
	}
	if len(bGot) != 0 {
		t.Fatalf("tenant B paraphrase delivered deltas %q: tenant A's answer must never replay to B", bGot)
	}
	// No laundering: B's failed attempt stored nothing under B.
	if _, ok := exact.Get(ctx, m611ScopedKey(t, se, bReq)); ok {
		t.Fatal("exact cache holds an entry under tenant B's key after B's failed paraphrase (laundered semantic hit)")
	}
}

// TestM611FixAnonymousTenantParaphrase proves BLOCKED-1 for the
// anonymous tenant in both directions: "" is a tenant like any
// other — a paraphrase from "" never serves a named tenant's
// answer, and a paraphrase from a named tenant never serves the
// anonymous tenant's answer.
func TestM611FixAnonymousTenantParaphrase(t *testing.T) {
	pairs := [][2]string{
		{"", "gk-named"},
		{"gk-named", ""},
	}
	ctx := context.Background()
	for _, pair := range pairs {
		f := &fakeEngine{chatResp: &provider.ChatResponse{
			Content: "Paris is the capital of France.",
			Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
			Model:   "test-model",
		}}
		se, _, _, _ := wrapSemanticTest(f, m611Policy())
		if _, err := se.Chat(ctx, m611Req(pair[0], "test-model", "What is the capital of France?", 0)); err != nil {
			t.Fatalf("tenants %q/%q: store Chat: %v", pair[0], pair[1], err)
		}
		m611FailProvider(f)
		resp, err := se.Chat(ctx, m611Req(pair[1], "test-model", "What is the capital of France today?", 0))
		if !errors.Is(err, errUpstreamDown) {
			t.Fatalf("tenants %q/%q: other-tenant paraphrase Chat = (%+v, %v), want the upstream error", pair[0], pair[1], resp, err)
		}
	}
}

// TestM611FixTempCrossTemp proves BLOCKED-2 on the Chat path
// with the allowance on: a temp-0 stream is never
// served the temp-0.7 answer (identical or paraphrase), while a
// temp-0.7 paraphrase still hits its own entry. It also pins the
// bucket edge semantically: 0.704 shares 0.700's entry, 0.71
// does not — and a failed temp-0 attempt launders nothing into
// the temp-0 exact key.
func TestM611FixTempCrossTemp(t *testing.T) {
	ctx := context.Background()
	pol := m611Policy()
	pol.AllowNonZeroTemp = true
	f := &fakeEngine{chatResp: &provider.ChatResponse{
		Content: "Paris is the capital of France.",
		Usage:   provider.Usage{PromptTokens: 8, CompletionTokens: 7, TotalTokens: 15},
		Model:   "test-model",
	}}
	se, exact, _, _ := wrapSemanticTest(f, pol)
	if _, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France?", 0.7)); err != nil {
		t.Fatalf("store temp-0.7 Chat: %v", err)
	}
	// Control: same-temperature paraphrase still hits its own
	// entry, without a provider call.
	if resp, err := se.Chat(ctx, m611Req("tenant-a", "test-model", "What is the capital of France today?", 0.7)); err != nil || resp == nil || resp.Content != "Paris is the capital of France." {
		t.Fatalf("same-temp paraphrase Chat = (%+v, %v), want the stored hot answer", resp, err)
	}
	if f.chatCalls != 1 {
		t.Fatalf("same-temp paraphrase chatCalls = %d, want 1 (semantic hit)", f.chatCalls)
	}
	m611FailProvider(f)
	// Property: temp-0 identical + paraphrase both fail.
	for _, text := range []string{"What is the capital of France?", "What is the capital of France today?"} {
		if resp, err := se.Chat(ctx, m611Req("tenant-a", "test-model", text, 0)); !errors.Is(err, errUpstreamDown) {
			t.Fatalf("temp-0 Chat text %q = (%+v, %v), want the upstream error", text, resp, err)
		}
	}
	// No laundering into the temp-0 exact key.
	if _, ok := exact.Get(ctx, m611ScopedKey(t, se, m611Req("tenant-a", "test-model", "What is the capital of France?", 0))); ok {
		t.Fatal("exact cache holds the hot answer under the temp-0 key after the failed temp-0 lookups (laundering)")
	}
	// Bucket edge, semantic: 0.704 shares 0.700's entry for a
	// paraphrase, 0.71 does not.
	f2 := &fakeEngine{chatResp: m611Resp("hot-answer", "stop")}
	se2, _, _, _ := wrapSemanticTest(f2, pol)
	if _, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2?", 0.700)); err != nil {
		t.Fatalf("store temp-0.700 Chat: %v", err)
	}
	if resp, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2 today?", 0.704)); err != nil || resp == nil || resp.Content != "hot-answer" {
		t.Fatalf("temp-0.704 paraphrase Chat = (%+v, %v), want the stored hot answer (same bucket shares)", resp, err)
	}
	if f2.chatCalls != 1 {
		t.Fatalf("0.704 paraphrase after 0.700 chatCalls = %d, want 1 (same bucket shares semantically)", f2.chatCalls)
	}
	f2.chatResp = m611Resp("other-bucket", "stop")
	if resp, err := se2.Chat(ctx, m611Req("tenant-a", "test-model", "What is 2+2 today?", 0.71)); err != nil || resp == nil || resp.Content != "other-bucket" {
		t.Fatalf("temp-0.71 paraphrase Chat = (%+v, %v), want a fresh answer (different bucket)", resp, err)
	}
	if f2.chatCalls != 2 {
		t.Fatalf("0.71 paraphrase after 0.700 chatCalls = %d, want 2 (different bucket must reach the provider)", f2.chatCalls)
	}
}
