package cache

// Phase 2 carry-over closure: Usage gained omitempty detail fields
// (cached_tokens, reasoning_tokens). Serialization must stay backward
// compatible: entries written before the fields existed decode with
// zeros, and new entries round-trip exactly.

import (
	"encoding/json"
	"testing"
)

func TestUsageDetailJSONRoundTripAndBackwardCompatibility(t *testing.T) {
	// Legacy payload (pre-detail fields) decodes without error, zeros.
	var legacy Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18}`), &legacy); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if legacy.PromptTokens != 12 || legacy.CachedTokens != 0 || legacy.ReasoningTokens != 0 {
		t.Fatalf("legacy = %+v, want 12/6/18 with zero details", legacy)
	}

	// New payload round-trips both detail fields.
	var got Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":12,"completion_tokens":6,"total_tokens":18,"cached_tokens":7,"reasoning_tokens":3}`), &got); err != nil {
		t.Fatalf("full decode: %v", err)
	}
	if got.CachedTokens != 7 || got.ReasoningTokens != 3 {
		t.Fatalf("details = %+v, want cached 7 / reasoning 3", got)
	}
}
