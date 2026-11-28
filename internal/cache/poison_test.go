// Tests for the store poison guard (Stream D): cache.StoreGuard.Poison
// is the gate every cache store path runs, and it must refuse refused,
// truncated, tool-call-carrying, empty and hot responses. The
// guard-removal sensitivity is proven behaviourally by the degrade and
// server store-site tests (poison_guard_test.go /
// cache_embedding_header_test.go): if Poison stops refusing, those tests
// fail because the poisoned response gets stored and later served.
package cache

import "testing"

func TestStoreGuardPoisonsEveryPoisonClass(t *testing.T) {
	cases := []struct {
		name string
		g    StoreGuard
		want StorePoison
	}{
		{"clean stop response", StoreGuard{FinishReason: "stop", Content: "4"}, StorePoisonNone},
		{"no finish reason", StoreGuard{Content: "4"}, StorePoisonNone},
		{"tool calls", StoreGuard{FinishReason: "tool_calls", Content: "", HasToolCalls: true}, StorePoisonToolCalls},
		{"tool calls with content", StoreGuard{FinishReason: "stop", Content: "calling a tool", HasToolCalls: true}, StorePoisonToolCalls},
		{"refusal content_filter", StoreGuard{FinishReason: "content_filter", Content: ""}, StorePoisonRefusal},
		{"refusal passthrough spelling", StoreGuard{FinishReason: "refusal", Content: "I cannot help with that."}, StorePoisonRefusal},
		{"refusal guardrail", StoreGuard{FinishReason: "guardrail_intervened", Content: ""}, StorePoisonRefusal},
		{"refusal safety", StoreGuard{FinishReason: "SAFETY", Content: ""}, StorePoisonRefusal},
		{"refusal case/dash variant", StoreGuard{FinishReason: "Content-Filter", Content: ""}, StorePoisonRefusal},
		{"truncated length", StoreGuard{FinishReason: "length", Content: "partial ans"}, StorePoisonTruncated},
		{"truncated max_tokens passthrough", StoreGuard{FinishReason: "max_tokens", Content: "partial ans"}, StorePoisonTruncated},
		{"truncated literal", StoreGuard{FinishReason: "truncated", Content: "partial"}, StorePoisonTruncated},
		{"truncated context window", StoreGuard{FinishReason: "model_context_window_exceeded", Content: "x"}, StorePoisonTruncated},
		{"empty content stop", StoreGuard{FinishReason: "stop", Content: ""}, StorePoisonEmpty},
		{"whitespace-only content", StoreGuard{FinishReason: "stop", Content: "  \t\n "}, StorePoisonEmpty},
		{"hot temperature", StoreGuard{FinishReason: "stop", Content: "4", Temperature: 0.7}, StorePoisonTemperature},
		{"hot temperature allowed", StoreGuard{FinishReason: "stop", Content: "4", Temperature: 0.7, AllowNonZeroTemp: true}, StorePoisonNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.g.Poison(); got != tc.want {
				t.Fatalf("Poison() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A refused response usually also has empty content: the refusal must
// win the label, not the empty check (the more useful reason).
func TestStoreGuardRefusalBeatsEmptyLabel(t *testing.T) {
	g := StoreGuard{FinishReason: "content_filter", Content: ""}
	if got := g.Poison(); got != StorePoisonRefusal {
		t.Fatalf("Poison() = %q, want refusal (not empty)", got)
	}
}

func TestIsRefusalFinish(t *testing.T) {
	yes := []string{"content_filter", "content_filtered", "refusal", "guardrail_intervened", "safety", "recitation", "blocked", "moderation", " INPUT_REFUSAL "}
	for _, r := range yes {
		if !IsRefusalFinish(r) {
			t.Errorf("IsRefusalFinish(%q) = false, want true", r)
		}
	}
	no := []string{"", "stop", "end_turn", "length", "tool_calls", "max_tokens", "done"}
	for _, r := range no {
		if IsRefusalFinish(r) {
			t.Errorf("IsRefusalFinish(%q) = true, want false", r)
		}
	}
}

func TestIsTruncatedFinish(t *testing.T) {
	yes := []string{"length", "max_tokens", "max_output_tokens", "truncated", "model_context_window_exceeded", "context_length_exceeded", " MAX_TOKENS "}
	for _, r := range yes {
		if !IsTruncatedFinish(r) {
			t.Errorf("IsTruncatedFinish(%q) = false, want true", r)
		}
	}
	no := []string{"", "stop", "end_turn", "tool_calls", "content_filter", "done"}
	for _, r := range no {
		if IsTruncatedFinish(r) {
			t.Errorf("IsTruncatedFinish(%q) = true, want false", r)
		}
	}
}

// ShouldStore (the legacy matrix) keeps its pinned behaviour: it
// refuses tool calls and finish "length" only. The store sites use the
// stricter StoreGuard, which also refuses the passthrough spellings -
// this test pins the deliberate difference so nobody "unifies" them
// and silently starts refusing max_tokens in the legacy matrix (or,
// the other way, stops refusing it at the store site).
func TestShouldStoreLegacyMatrixUnchanged(t *testing.T) {
	if !ShouldStore("stop", 0, false, false) {
		t.Error("ShouldStore(stop) must stay true")
	}
	if ShouldStore("length", 0, false, false) {
		t.Error("ShouldStore(length) must stay false")
	}
	if !ShouldStore("max_tokens", 0, false, false) {
		t.Error("ShouldStore(max_tokens) is pinned true by TestM611ShouldStoreMatrix; the store-site guard refuses it instead")
	}
	if ShouldStore("stop", 0, false, true) {
		t.Error("ShouldStore(tool calls) must stay false")
	}
}
