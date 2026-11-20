package compress

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestDefaultRegistryOrdered tests that DefaultRegistry contains all 15 engines
// and orders them deterministically by StackPriority.
func TestDefaultRegistryOrdered(t *testing.T) {
	reg := DefaultRegistry()
	ordered := reg.Ordered()
	if len(ordered) < 10 {
		t.Fatalf("expected at least 10 registered engines, got %d", len(ordered))
	}

	for i := 1; i < len(ordered); i++ {
		if ordered[i].StackPriority < ordered[i-1].StackPriority {
			t.Fatalf("registry ordering violated at %d: %s (prio %d) < %s (prio %d)",
				i, ordered[i].ID, ordered[i].StackPriority, ordered[i-1].ID, ordered[i-1].StackPriority)
		}
	}

	// Verify key engines are present
	for _, id := range []string{
		EngineLite, EngineSessionDedup, EngineCCR, EngineRTK, EngineCaveman,
		EngineCodex, EngineIonizer, EngineSmartCrusher, EngineRelevance,
		EngineStyleTerse, EngineStyleLessCode, EngineStylePonytail, EngineStyleADHD, EngineStyleTerseCJK,
	} {
		if _, ok := reg.Get(id); !ok {
			t.Errorf("expected engine %q to be registered in DefaultRegistry", id)
		}
	}
}

// TestApplyWithEnginesSmartCrusher tests running SmartCrusher losslessly through ApplyWithEngines.
func TestApplyWithEnginesSmartCrusher(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: `[{"name":"Alice","age":30},{"name":"Bob","age":25}]`},
	}
	ctx := EngineContext{}
	out, res := ApplyWithEngines("model-a", msgs, nil, nil, nil, []string{EngineSmartCrusher}, ctx)
	if !res.Applied {
		t.Fatal("ApplyWithEngines did not apply")
	}
	if len(out) == 0 {
		t.Fatal("empty output")
	}
	content := out[0].Content
	if !containsStr(content, "[smartcrusher:columns") {
		t.Fatalf("expected columnar table in content, got: %s", content)
	}
}

// TestApplyWithEnginesSessionDedup tests cross-turn deduplication.
func TestApplyWithEnginesSessionDedup(t *testing.T) {
	store := NewSessionDedupStore()
	sessionID := "sess-123"
	ctx := EngineContext{
		DedupStore: store,
		SessionID:  sessionID,
	}

	repeatedContent := "System instruction repeated block that is identical across turns"
	msgs1 := []provider.Message{
		{Role: "system", Content: repeatedContent},
		{Role: "user", Content: "Hello"},
	}
	out1, _ := ApplyWithEngines("model-a", msgs1, nil, nil, nil, []string{EngineSessionDedup}, ctx)
	if out1[0].Content != repeatedContent {
		t.Fatalf("first occurrence should not be replaced, got: %s", out1[0].Content)
	}

	msgs2 := []provider.Message{
		{Role: "system", Content: repeatedContent},
		{Role: "user", Content: "Hello again"},
	}
	out2, _ := ApplyWithEngines("model-a", msgs2, nil, nil, nil, []string{EngineSessionDedup}, ctx)
	if !containsStr(out2[0].Content, "[dedup:ref sha=") {
		t.Fatalf("second occurrence should be replaced by dedup marker, got: %s", out2[0].Content)
	}
}

// TestApplyWithEnginesCCR tests contextual compression retrieval with SQLite store.
func TestApplyWithEnginesCCR(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ccr.db")
	store, err := OpenCCRStore(dbPath)
	if err != nil {
		t.Fatalf("OpenCCRStore: %v", err)
	}
	defer store.Close()

	ctx := EngineContext{
		CCRStore: store,
	}

	repeatedContent := "Contextual compression retrieval retrieved document chunk 123456"
	msgs1 := []provider.Message{
		{Role: "assistant", Content: repeatedContent},
		{Role: "user", Content: "query 1"},
	}
	out1, _ := ApplyWithEngines("model-a", msgs1, nil, nil, nil, []string{EngineCCR}, ctx)
	if out1[0].Content != repeatedContent {
		t.Fatalf("first occurrence should remain, got: %s", out1[0].Content)
	}

	msgs2 := []provider.Message{
		{Role: "assistant", Content: repeatedContent},
		{Role: "user", Content: "query 2"},
	}
	out2, _ := ApplyWithEngines("model-a", msgs2, nil, nil, nil, []string{EngineCCR}, ctx)
	if !containsStr(out2[0].Content, "[ccr:ref sha=") {
		t.Fatalf("second occurrence should be replaced by ccr marker, got: %s", out2[0].Content)
	}
}

// TestApplyWithEnginesStyleTerse tests lossy prose condensation under lossy opt-in.
func TestApplyWithEnginesStyleTerse(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "It is important to note that this is really definitely very good."},
	}
	req := &provider.CompressionRequest{
		LossyMaxPromptTokens: 50,
	}
	ctx := EngineContext{}
	out, res := ApplyWithEngines("model-a", msgs, req, nil, nil, []string{EngineStyleTerse}, ctx)
	if !res.Applied {
		t.Fatal("ApplyWithEngines did not apply")
	}
	if len(out) == 0 {
		t.Fatal("empty output")
	}
	if containsStr(out[0].Content, "It is important to note that") {
		t.Fatalf("filler was not stripped: %s", out[0].Content)
	}
}

// TestApplyWithEnginesCodex tests Codex Responses tool output compression.
func TestApplyWithEnginesCodex(t *testing.T) {
	msgs := []provider.Message{
		{Role: "tool", ToolCallID: "call_1", Content: "\x1b[31mError:\x1b[0m test failure line 1\n\x1b[31mError:\x1b[0m test failure line 2"},
	}
	req := &provider.CompressionRequest{LossyMaxPromptTokens: 50}
	ctx := EngineContext{CodexMaxChars: 500}
	out, res := ApplyWithEngines("model-a", msgs, req, nil, nil, []string{EngineCodex}, ctx)
	if !res.Applied {
		t.Fatal("ApplyWithEngines did not apply")
	}
	if containsStr(out[0].Content, "\x1b[31m") {
		t.Fatalf("ANSI escape codes not stripped: %s", out[0].Content)
	}
}

// TestApplyWithEnginesIonizer tests tabular row sampling.
func TestApplyWithEnginesIonizer(t *testing.T) {
	var rows []string
	for i := 0; i < 25; i++ {
		rows = append(rows, `{"id":`+itoa(i)+`,"val":"x"}`)
	}
	tableJSON := "[" + strings.Join(rows, ",") + "]"
	msgs := []provider.Message{
		{Role: "user", Content: tableJSON},
	}
	req := &provider.CompressionRequest{LossyMaxPromptTokens: 100}
	ctx := EngineContext{}
	out, res := ApplyWithEngines("model-a", msgs, req, nil, nil, []string{EngineIonizer}, ctx)
	if !res.Applied {
		t.Fatal("ApplyWithEngines did not apply")
	}
	if !containsStr(out[0].Content, "[ionizer: kept 12 of 25 rows]") {
		t.Fatalf("expected ionizer marker in content, got: %s", out[0].Content)
	}
}

// TestApplyWithEnginesRelevance tests extractive sentence scoring.
func TestApplyWithEnginesRelevance(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "First important sentence about database storage. Irrelevant filler sentence here. Another completely unrelated thought. Final important sentence about database storage."},
	}
	req := &provider.CompressionRequest{LossyMaxPromptTokens: 50}
	ctx := EngineContext{}
	out, res := ApplyWithEngines("model-a", msgs, req, nil, nil, []string{EngineRelevance}, ctx)
	if !res.Applied {
		t.Fatal("ApplyWithEngines did not apply")
	}
	// First and last sentences are always preserved
	if !containsStr(out[0].Content, "First important sentence") || !containsStr(out[0].Content, "Final important sentence") {
		t.Fatalf("first or last sentence missing: %s", out[0].Content)
	}
}

// TestAgeMessagesProgressive tests progressive aging on turns outside the recency window.
func TestAgeMessagesProgressive(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "System prompt"},
		{Role: "user", Content: "Old long query with lots of detail that happened turns ago: abcdefghijklmnopqrstuvwxyz0123456789"},
		{Role: "assistant", Content: "Old long response that happened turns ago: abcdefghijklmnopqrstuvwxyz0123456789"},
		{Role: "user", Content: "Recent query 1"},
		{Role: "assistant", Content: "Recent response 1"},
		{Role: "user", Content: "Recent query 2"},
		{Role: "assistant", Content: "Recent response 2"},
	}
	aged, changed := AgeMessages(msgs, 4)
	if changed == 0 {
		t.Fatal("expected older turns to be aged")
	}
	if aged[0].Content != "System prompt" {
		t.Fatal("system prompt must never be aged")
	}
	if aged[len(aged)-1].Content != "Recent response 2" {
		t.Fatal("most recent turn must not be aged")
	}
	if !containsStr(aged[1].Content, truncationMarker) && !containsStr(aged[2].Content, truncationMarker) {
		t.Fatalf("expected middle truncation in aged messages: %v", aged)
	}
}

// TestPruneHeuristicCodeThinning tests code thinning and binary search truncation.
func TestPruneHeuristicCodeThinning(t *testing.T) {
	codeWithComments := "```go\n// comment 1\npackage main\n\n// comment 2\nfunc main() {}\n```"
	msgs := []provider.Message{
		{Role: "user", Content: "Instruction"},
		{Role: "assistant", Content: codeWithComments},
		{Role: "user", Content: "Next instruction"},
	}
	pruned, _, thinned := PruneHeuristic("model-a", msgs, 1000)
	if thinned == 0 {
		t.Fatal("expected code comments to be thinned")
	}
	if containsStr(pruned[1].Content, "// comment 1") {
		t.Fatalf("comment was not stripped: %s", pruned[1].Content)
	}
	if !containsStr(pruned[1].Content, "package main") {
		t.Fatalf("code was dropped: %s", pruned[1].Content)
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
