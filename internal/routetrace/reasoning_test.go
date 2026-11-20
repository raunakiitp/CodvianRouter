package routetrace

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAddWithReasoning proves the reasoning handoff label rides
// the attempt record and its JSON, and is omitted when empty.
func TestAddWithReasoning(t *testing.T) {
	tr := &RouteTrace{}
	tr.AddWithReasoning("anthropic", "claude-sonnet-4-5", 42, "success", nil, "", "preserved: 1 thinking block(s) replayed verbatim")
	tr.AddWithReasoning("gemini", "gemini-2.5-pro", 100, "success", nil, "", "")
	if len(tr.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(tr.Attempts))
	}
	if tr.Attempts[0].Reasoning != "preserved: 1 thinking block(s) replayed verbatim" {
		t.Fatalf("reasoning = %q", tr.Attempts[0].Reasoning)
	}
	if tr.Attempts[1].Reasoning != "" {
		t.Fatalf("reasoning = %q, want empty", tr.Attempts[1].Reasoning)
	}
	b, err := json.Marshal(tr.Attempts[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"reasoning":"preserved:`) {
		t.Fatalf("json = %s, want the reasoning field", b)
	}
	b2, err := json.Marshal(tr.Attempts[1])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b2), "reasoning") {
		t.Fatalf("json = %s, want no reasoning field when empty", b2)
	}
}
