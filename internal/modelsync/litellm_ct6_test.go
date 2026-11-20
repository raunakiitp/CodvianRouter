package modelsync

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// CT-6 (2026-11-19): LiteLLM parse must not misattribute a model
// when the `litellm_provider` field disagrees with the key prefix,
// and duplicate (provider, model) pairs must resolve
// deterministically with a warning. Chosen rules (fail-closed,
// documented on ParseLiteLLMCatalog): mismatch -> REJECT the entry
// (warning logged, never lands in the catalog); duplicate ->
// FIRST-WINS in sorted-key order (warning logged). Warnings are
// captured via the injectable-logger parse variant.

// TestCT6LiteLLMMismatchRejected: the brief's example —
// `deepseek/deepseek-chat` + field `"qoder"` — must never land in
// the parsed catalog, under either provider.
func TestCT6LiteLLMMismatchRejected(t *testing.T) {
	const mismatchJSON = `{
  "deepseek/deepseek-chat": {"input_cost_per_token": 0.00000014, "output_cost_per_token": 0.00000028, "litellm_provider": "qoder", "max_input_tokens": 64000},
  "openai/gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001, "litellm_provider": "openai", "max_input_tokens": 128000}
}`
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cat, err := parseLiteLLMCatalog([]byte(mismatchJSON), log)
	if err != nil {
		t.Fatalf("parseLiteLLMCatalog: %v", err)
	}
	if _, ok := cat.Providers["qoder"]; ok {
		t.Error("mismatched entry landed under the field provider \"qoder\"")
	}
	if deepseek, ok := cat.Providers["deepseek"]; ok {
		if _, ok := deepseek.Models["deepseek-chat"]; ok {
			t.Error("mismatched entry landed under the key-prefix provider \"deepseek\"")
		}
	}
	if len(cat.Providers) != 1 {
		t.Fatalf("providers = %d, want 1 (only the agreeing openai entry)", len(cat.Providers))
	}
	if _, ok := cat.Providers["openai"].Models["gpt-4o"]; !ok {
		t.Error("agreeing entry openai/gpt-4o did not parse")
	}
	if !strings.Contains(buf.String(), "provider mismatch") {
		t.Errorf("no mismatch warning logged, log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "qoder") || !strings.Contains(buf.String(), "deepseek/deepseek-chat") {
		t.Errorf("mismatch warning does not name the entry, log = %q", buf.String())
	}
}

// TestCT6LiteLLMMismatchRejectedViaExportedParse: the same
// rejection through the exported entry point (the Sync path's
// caller), without a captured logger.
func TestCT6LiteLLMMismatchRejectedViaExportedParse(t *testing.T) {
	const mismatchJSON = `{
  "deepseek/deepseek-chat": {"input_cost_per_token": 0.00000014, "output_cost_per_token": 0.00000028, "litellm_provider": "qoder", "max_input_tokens": 64000}
}`
	cat, err := ParseLiteLLMCatalog([]byte(mismatchJSON))
	if err != nil {
		t.Fatalf("ParseLiteLLMCatalog: %v", err)
	}
	if len(cat.Providers) != 0 {
		t.Fatalf("providers = %d, want 0 (sole entry is mismatched and rejected)", len(cat.Providers))
	}
}

// TestCT6LiteLLMFieldAgreesWithPrefixParses: the negative test —
// field == prefix parses normally, including the bare-key form
// (no prefix, field alone) and the empty-field form (prefix alone).
func TestCT6LiteLLMFieldAgreesWithPrefixParses(t *testing.T) {
	const agreeJSON = `{
  "deepseek/deepseek-chat": {"input_cost_per_token": 0.00000014, "output_cost_per_token": 0.00000028, "litellm_provider": "deepseek", "max_input_tokens": 64000},
  "gpt-4o-mini": {"input_cost_per_token": 1.5e-07, "output_cost_per_token": 6e-07, "litellm_provider": "openai", "max_input_tokens": 128000},
  "anthropic/claude-3": {"input_cost_per_token": 0.000003, "output_cost_per_token": 0.000015, "max_input_tokens": 200000}
}`
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cat, err := parseLiteLLMCatalog([]byte(agreeJSON), log)
	if err != nil {
		t.Fatalf("parseLiteLLMCatalog: %v", err)
	}
	if _, ok := cat.Providers["deepseek"].Models["deepseek-chat"]; !ok {
		t.Error("agreeing entry deepseek/deepseek-chat did not parse")
	}
	if _, ok := cat.Providers["openai"].Models["gpt-4o-mini"]; !ok {
		t.Error("bare-key entry gpt-4o-mini did not parse under its field provider")
	}
	if _, ok := cat.Providers["anthropic"].Models["claude-3"]; !ok {
		t.Error("empty-field entry anthropic/claude-3 did not parse under its key prefix")
	}
	if buf.Len() != 0 {
		t.Errorf("agreeing entries produced warnings, log = %q", buf.String())
	}
}

// TestCT6LiteLLMDuplicateFirstWinsWarns: two keys mapping to the
// same (provider, model) pair — bare "gpt-4o" (field openai) and
// "openai/gpt-4o" — resolve FIRST-WINS in sorted-key order
// ("gpt-4o" < "openai/gpt-4o"), with a warning, and the loser's
// price never overwrites the winner's.
func TestCT6LiteLLMDuplicateFirstWinsWarns(t *testing.T) {
	const dupJSON = `{
  "gpt-4o": {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000002, "litellm_provider": "openai", "max_input_tokens": 128000},
  "openai/gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001, "litellm_provider": "openai", "max_input_tokens": 128000}
}`
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cat, err := parseLiteLLMCatalog([]byte(dupJSON), log)
	if err != nil {
		t.Fatalf("parseLiteLLMCatalog: %v", err)
	}
	openai := cat.Providers["openai"]
	if len(openai.Models) != 1 {
		t.Fatalf("openai models = %d, want 1 (duplicate pair collapses to one)", len(openai.Models))
	}
	got := openai.Models["gpt-4o"]
	// First-wins: the bare key sorts first, so its 1.0/2.0 per-1M
	// prices win over the prefixed key's 2.5/10.
	if got.InputUSDPer1M == nil || *got.InputUSDPer1M != 1.0 ||
		got.OutputUSDPer1M == nil || *got.OutputUSDPer1M != 2.0 {
		t.Errorf("gpt-4o price = %v/%v per 1M, want first-wins 1.0/2.0 (bare key)", got.InputUSDPer1M, got.OutputUSDPer1M)
	}
	if !strings.Contains(buf.String(), "duplicate") {
		t.Errorf("no duplicate warning logged, log = %q", buf.String())
	}
	if !strings.Contains(buf.String(), "openai/gpt-4o") {
		t.Errorf("duplicate warning does not name the losing key, log = %q", buf.String())
	}
	// Deterministic: parse twice, same winner.
	cat2, err := parseLiteLLMCatalog([]byte(dupJSON), log)
	if err != nil {
		t.Fatalf("second parseLiteLLMCatalog: %v", err)
	}
	again := cat2.Providers["openai"].Models["gpt-4o"]
	if again.InputUSDPer1M == nil || *again.InputUSDPer1M != 1.0 {
		t.Errorf("duplicate resolution not deterministic: second parse input = %v, want 1.0", again.InputUSDPer1M)
	}
}
