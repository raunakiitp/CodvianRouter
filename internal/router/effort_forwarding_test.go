// Intent-tier effort forwarding on non-OpenAI wires (CHECKLIST
// 285-286, Vertex 303-304): the engine resolves the tier's
// effort through the operator-declared effort→budget table and
// hands the numeric budget to the Anthropic/Gemini/Vertex
// attempt; with no table (or no entry) nothing is sent and the
// trace says so; Cohere/Bedrock keep their honest "no effort
// control".
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestResolveEffortForwarding: the pure resolver's label for every
// wire, with and without a table.
func TestResolveEffortForwarding(t *testing.T) {
	table := func(wire, effort string) (int, bool) {
		if wire == "anthropic" && effort == "low" {
			return 1024, true
		}
		if wire == "gemini" && effort == "high" {
			return 8192, true
		}
		if wire == "vertex" && effort == "low" {
			return 1024, true
		}
		if wire == "vertex" && effort == "high" {
			return 16384, true
		}
		return 0, false
	}
	for _, tc := range []struct {
		wire, effort string
		lookup       func(string, string) (int, bool)
		budget       int
		label        string
	}{
		{"anthropic", "low", table, 1024, "forwarded:thinking.budget_tokens=1024"},
		{"anthropic", "high", table, 0, "not-forwarded:no-operator-budget-table"},
		{"anthropic", "low", nil, 0, "not-forwarded:no-operator-budget-table"},
		{"gemini", "high", table, 8192, "forwarded:thinkingConfig.thinkingBudget=8192"},
		{"gemini", "low", table, 0, "not-forwarded:no-operator-budget-table"},
		{"gemini", "low", nil, 0, "not-forwarded:no-operator-budget-table"},
		{"openai", "low", nil, 0, "forwarded:reasoning_effort=low"},
		{"openai", "high", table, 0, "forwarded:reasoning_effort=high"},
		{"ollama", "low", nil, 0, "forwarded:reasoning_effort=low"},
		{"cohere", "low", table, 0, "not-forwarded:no-effort-control"},
		{"bedrock-converse", "high", table, 0, "not-forwarded:no-effort-control"},
		{"vertex", "low", table, 1024, "forwarded:thinkingConfig.thinkingBudget=1024"},
		{"vertex", "high", table, 16384, "forwarded:thinkingConfig.thinkingBudget=16384"},
		{"vertex", "medium", table, 0, "not-forwarded:no-operator-budget-table"},
		{"vertex", "low", nil, 0, "not-forwarded:no-operator-budget-table"},
		{"", "low", table, 0, "not-forwarded:unknown-wire"},
	} {
		budget, label := resolveEffortForwarding(tc.wire, tc.effort, tc.lookup)
		if budget != tc.budget || label != tc.label {
			t.Fatalf("resolveEffortForwarding(%q, %q) = (%d, %q), want (%d, %q)",
				tc.wire, tc.effort, budget, label, tc.budget, tc.label)
		}
	}
}

// budgetEngine builds an engine with one fake client speaking the
// given wire, resolving intent/* to model at effort, and with the
// given effort→budget lookup (nil = no table declared). The fake
// records every ChatRequest it receives, so the test can prove the
// numeric budget the attempt carried.
func budgetEngine(t *testing.T, wire, model, effort string, lookup func(string, string) (int, bool)) (*Engine, *fakeClient) {
	t.Helper()
	a := &fakeClient{wireFormat: wire}
	cfg := testConfig()
	cfg.AutoRoute = stubIntentRoute(model, effort)
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	cfg.EffortBudgetFor = lookup
	return NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg), a
}

func tableLookup(t *testing.T) func(string, string) (int, bool) {
	t.Helper()
	return func(wire, effort string) (int, bool) {
		budgets := map[string]map[string]int{
			"anthropic": {"low": 1024, "high": 16384},
			"gemini":    {"low": 2048, "high": 8192},
			"vertex":    {"low": 1024, "high": 16384},
		}
		if m, ok := budgets[wire]; ok {
			b, ok := m[effort]
			return b, ok
		}
		return 0, false
	}
}

// TestEffortBudgetReachesAnthropicAttempt: intent/fast (low) on an
// Anthropic wire carries ThinkingBudget 1024 to the client, and
// the trace records the forwarded value; intent/best (high)
// carries 16384 — the budgets differ by tier, exactly as the
// operator's table says.
func TestEffortBudgetReachesAnthropicAttempt(t *testing.T) {
	for _, tc := range []struct {
		model  string
		budget int
		label  string
	}{
		{"intent/fast", 1024, "forwarded:thinking.budget_tokens=1024"},
		{"intent/best", 16384, "forwarded:thinking.budget_tokens=16384"},
	} {
		eng, a := budgetEngine(t, provider.WireFormatAnthropic, "model-a", map[string]string{"intent/fast": "low", "intent/best": "high"}[tc.model], tableLookup(t))
		req := testReq()
		req.Model = tc.model
		if _, err := eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat %s: %v", tc.model, err)
		}
		if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != tc.budget {
			t.Fatalf("%s: client ThinkingBudget = %v, want [%d]", tc.model, a.reqs, tc.budget)
		}
		tr := eng.LastTrace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != tc.label {
			t.Fatalf("%s: trace attempts = %v, want one attempt with effort_forwarding %q", tc.model, tr, tc.label)
		}
	}
}

// TestEffortBudgetReachesVertexAttempt: the same proof on the
// Vertex wire (CHECKLIST 303-304) — intent/fast (low) carries
// 1024 and intent/best (high) 16384 as
// thinkingConfig.thinkingBudget, the budgets differing by tier
// exactly as the operator's "vertex" table entry says. A
// "gemini" entry alone must NOT leak onto Vertex: the lookup
// below maps vertex only, and the engine test with a
// gemini-only table proves the attempt sends nothing.
func TestEffortBudgetReachesVertexAttempt(t *testing.T) {
	for _, tc := range []struct {
		model  string
		budget int
		label  string
	}{
		{"intent/fast", 1024, "forwarded:thinkingConfig.thinkingBudget=1024"},
		{"intent/best", 16384, "forwarded:thinkingConfig.thinkingBudget=16384"},
	} {
		eng, a := budgetEngine(t, provider.WireFormatVertex, "model-v", map[string]string{"intent/fast": "low", "intent/best": "high"}[tc.model], tableLookup(t))
		req := testReq()
		req.Model = tc.model
		if _, err := eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat %s: %v", tc.model, err)
		}
		if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != tc.budget {
			t.Fatalf("%s: client ThinkingBudget = %v, want [%d]", tc.model, a.reqs, tc.budget)
		}
		tr := eng.LastTrace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != tc.label {
			t.Fatalf("%s: trace attempts = %v, want one attempt with effort_forwarding %q", tc.model, tr, tc.label)
		}
	}
}

// TestEffortBudgetVertexGeminiEntryDoesNotApply: the table maps
// gemini only — the Vertex attempt sends no budget and the trace
// says the table had no entry for it, proving a "gemini" budget
// is never copied onto Vertex.
func TestEffortBudgetVertexGeminiEntryDoesNotApply(t *testing.T) {
	lookup := func(wire, effort string) (int, bool) {
		if wire == "gemini" && effort == "low" {
			return 2048, true
		}
		return 0, false
	}
	eng, a := budgetEngine(t, provider.WireFormatVertex, "model-v", "low", lookup)
	req := testReq()
	req.Model = "intent/fast"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 0 {
		t.Fatalf("client ThinkingBudget = %v, want [0] (gemini-only table)", a.reqs)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "not-forwarded:no-operator-budget-table" {
		t.Fatalf("trace attempts = %v, want effort_forwarding not-forwarded:no-operator-budget-table", tr)
	}
}

// TestEffortBudgetVertexAbsentSendsNothing: with no table
// declared the Vertex attempt sends no budget and the trace
// records the gap honestly.
func TestEffortBudgetVertexAbsentSendsNothing(t *testing.T) {
	eng, a := budgetEngine(t, provider.WireFormatVertex, "model-v", "low", nil)
	req := testReq()
	req.Model = "intent/fast"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 0 {
		t.Fatalf("client ThinkingBudget = %v, want [0] (no table)", a.reqs)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "not-forwarded:no-operator-budget-table" {
		t.Fatalf("trace attempts = %v, want effort_forwarding not-forwarded:no-operator-budget-table", tr)
	}
}

// TestEffortBudgetVertexStreamPath: the streaming path carries
// the same resolved budget to the client and the same label to
// the trace.
func TestEffortBudgetVertexStreamPath(t *testing.T) {
	eng, a := budgetEngine(t, provider.WireFormatVertex, "model-v", "low", tableLookup(t))
	req := testReq()
	req.Model = "intent/fast"
	req.Stream = true
	_, err := eng.ChatStream(context.Background(), req, func(string) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	sreq, ok := a.lastStreamReq()
	if !ok || sreq.ThinkingBudget != 1024 {
		t.Fatalf("stream client ThinkingBudget = %v (ok=%v), want 1024", sreq.ThinkingBudget, ok)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "forwarded:thinkingConfig.thinkingBudget=1024" {
		t.Fatalf("trace attempts = %v, want effort_forwarding forwarded:thinkingConfig.thinkingBudget=1024", tr)
	}
}

// TestEffortBudgetReachesGeminiAttempt: the same proof on the
// Gemini wire — intent/best (high) carries 8192 as
// thinkingConfig.thinkingBudget.
func TestEffortBudgetReachesGeminiAttempt(t *testing.T) {
	eng, a := budgetEngine(t, provider.WireFormatGemini, "model-g", "high", tableLookup(t))
	req := testReq()
	req.Model = "intent/best"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 8192 {
		t.Fatalf("client ThinkingBudget = %v, want [8192]", a.reqs)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "forwarded:thinkingConfig.thinkingBudget=8192" {
		t.Fatalf("trace attempts = %v, want effort_forwarding forwarded:thinkingConfig.thinkingBudget=8192", tr)
	}
}

// TestEffortBudgetAbsentSendsNothing: with no table declared the
// Anthropic attempt sends no budget (ThinkingBudget 0) and the
// trace records the gap honestly instead of inventing a number.
func TestEffortBudgetAbsentSendsNothing(t *testing.T) {
	eng, a := budgetEngine(t, provider.WireFormatAnthropic, "model-a", "low", nil)
	req := testReq()
	req.Model = "intent/fast"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 0 {
		t.Fatalf("client ThinkingBudget = %v, want [0] (no table)", a.reqs)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "not-forwarded:no-operator-budget-table" {
		t.Fatalf("trace attempts = %v, want effort_forwarding not-forwarded:no-operator-budget-table", tr)
	}
}

// TestEffortBudgetUnmappedEffortSendsNothing: the table exists but
// has no entry for this effort — again nothing is sent, and the
// trace says the table had no entry.
func TestEffortBudgetUnmappedEffortSendsNothing(t *testing.T) {
	// The table maps low only; the tier pins medium.
	lookup := func(wire, effort string) (int, bool) {
		if wire == "anthropic" && effort == "low" {
			return 1024, true
		}
		return 0, false
	}
	eng, a := budgetEngine(t, provider.WireFormatAnthropic, "model-a", "medium", lookup)
	req := testReq()
	req.Model = "intent/fast"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 0 {
		t.Fatalf("client ThinkingBudget = %v, want [0] (unmapped effort)", a.reqs)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "not-forwarded:no-operator-budget-table" {
		t.Fatalf("trace attempts = %v, want effort_forwarding not-forwarded:no-operator-budget-table", tr)
	}
}

// TestEffortBudgetNoEffortControlWires: Cohere and Bedrock keep
// their honest "no effort control" — the table is never consulted
// for them (even a table that claimed an entry would not apply),
// and no budget is sent.
func TestEffortBudgetNoEffortControlWires(t *testing.T) {
	for _, wire := range []string{provider.WireFormatCohere, provider.WireFormatBedrock} {
		eng, a := budgetEngine(t, wire, "model-c", "high", tableLookup(t))
		req := testReq()
		req.Model = "intent/best"
		if _, err := eng.Chat(context.Background(), req); err != nil {
			t.Fatalf("Chat %s: %v", wire, err)
		}
		if len(a.reqs) != 1 || a.reqs[0].ThinkingBudget != 0 {
			t.Fatalf("%s: client ThinkingBudget = %v, want [0]", wire, a.reqs)
		}
		tr := eng.LastTrace()
		if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "not-forwarded:no-effort-control" {
			t.Fatalf("%s: trace attempts = %v, want effort_forwarding not-forwarded:no-effort-control", wire, tr)
		}
	}
}

// TestEffortBudgetStreamPath: the streaming path carries the same
// resolved budget to the client and the same label to the trace.
func TestEffortBudgetStreamPath(t *testing.T) {
	eng, a := budgetEngine(t, provider.WireFormatAnthropic, "model-a", "low", tableLookup(t))
	req := testReq()
	req.Model = "intent/fast"
	req.Stream = true
	_, err := eng.ChatStream(context.Background(), req, func(string) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	sreq, ok := a.lastStreamReq()
	if !ok || sreq.ThinkingBudget != 1024 {
		t.Fatalf("stream client ThinkingBudget = %v (ok=%v), want 1024", sreq.ThinkingBudget, ok)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].EffortForwarding != "forwarded:thinking.budget_tokens=1024" {
		t.Fatalf("trace attempts = %v, want effort_forwarding forwarded:thinking.budget_tokens=1024", tr)
	}
}
