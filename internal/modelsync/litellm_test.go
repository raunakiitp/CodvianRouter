package modelsync

import "testing"

// testLiteLLMJSON is a small LiteLLM model_prices_and_context_window.json
// document: one full entry (provider in key prefix), one with the
// provider in litellm_provider, the sample_spec template entry, one
// with no attributable provider, one with an invalid (negative)
// price, one deprecated in the past, one deprecated in the future.
const testLiteLLMJSON = `{
  "sample_spec": {"input_cost_per_token": 0.0, "litellm_provider": "openai", "max_input_tokens": 1, "max_output_tokens": 1, "mode": "chat"},
  "openai/gpt-4o": {"input_cost_per_token": 0.0000025, "output_cost_per_token": 0.00001, "cache_read_input_token_cost": 0.00000125, "litellm_provider": "openai", "max_input_tokens": 128000, "max_output_tokens": 16384, "mode": "chat"},
  "gpt-4o-mini": {"input_cost_per_token": 1.5e-07, "output_cost_per_token": 6e-07, "litellm_provider": "openai", "max_input_tokens": 128000, "mode": "chat"},
  "unattributed-model": {"input_cost_per_token": 1e-06, "output_cost_per_token": 1e-06, "max_input_tokens": 8000},
  "anthropic/claude-bad": {"input_cost_per_token": -5, "output_cost_per_token": 15, "litellm_provider": "anthropic", "max_input_tokens": 8192},
  "openai/gpt-3.5-turbo-0301": {"input_cost_per_token": 0.0000015, "output_cost_per_token": 0.000002, "litellm_provider": "openai", "max_input_tokens": 16385, "deprecation_date": "2024-09-13"},
  "openai/gpt-next": {"input_cost_per_token": 0.000001, "output_cost_per_token": 0.000002, "litellm_provider": "openai", "max_input_tokens": 100000, "deprecation_date": "2099-01-01"}
}`

// TestParseLiteLLMCatalog — parity tracker:505, PARTIAL row: models.dev
// sync is the only sync source; the reference router also syncs LiteLLM.
// This parses LiteLLM's cost map into the same Catalog shape so
// one Plan can run against either source. Fetch/schedule wiring
// for the second source is follow-up work.
func TestParseLiteLLMCatalog(t *testing.T) {
	cat, err := ParseLiteLLMCatalog([]byte(testLiteLLMJSON))
	if err != nil {
		t.Fatalf("ParseLiteLLMCatalog: %v", err)
	}
	if len(cat.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 (openai, anthropic)", len(cat.Providers))
	}
	openai := cat.Providers["openai"]
	if len(openai.Models) != 4 {
		t.Fatalf("openai models = %d, want 4 (sample_spec + unattributed excluded)", len(openai.Models))
	}
	// Per-token prices convert to per-1M (x1e6).
	full := openai.Models["gpt-4o"]
	if full.ContextWindow != 128000 || full.InputUSDPer1M == nil || *full.InputUSDPer1M != 2.5 ||
		full.OutputUSDPer1M == nil || *full.OutputUSDPer1M != 10 ||
		full.CachedInputUSDPer1M == nil || *full.CachedInputUSDPer1M != 1.25 {
		t.Errorf("gpt-4o = %+v, want window 128000 + 2.5/10/1.25 per 1M", full)
	}
	// A bare key with litellm_provider attributes to that provider.
	mini := openai.Models["gpt-4o-mini"]
	if mini.InputUSDPer1M == nil || *mini.InputUSDPer1M != 0.15 || mini.OutputUSDPer1M == nil || *mini.OutputUSDPer1M != 0.6 {
		t.Errorf("gpt-4o-mini price = %v/%v, want 0.15/0.6", mini.InputUSDPer1M, mini.OutputUSDPer1M)
	}
	// Invalid price: CostInvalid, window still kept.
	bad := cat.Providers["anthropic"].Models["claude-bad"]
	if !bad.CostInvalid {
		t.Error("negative-cost model not marked CostInvalid")
	}
	if bad.ContextWindow != 8192 {
		t.Error("invalid-price model lost its window")
	}
	// Deprecation dates: past = deprecated, future = not.
	if !openai.Models["gpt-3.5-turbo-0301"].Deprecated {
		t.Error("past deprecation_date not marked deprecated")
	}
	if openai.Models["gpt-next"].Deprecated {
		t.Error("future deprecation_date marked deprecated")
	}
	// Deterministic: parse twice, same shape.
	cat2, err := ParseLiteLLMCatalog([]byte(testLiteLLMJSON))
	if err != nil {
		t.Fatalf("second ParseLiteLLMCatalog: %v", err)
	}
	if len(cat2.Providers) != len(cat.Providers) || len(cat2.Providers["openai"].Models) != len(openai.Models) {
		t.Error("LiteLLM parse is not deterministic across runs")
	}
	if _, err := ParseLiteLLMCatalog([]byte(`{not json`)); err == nil {
		t.Error("malformed JSON parsed without error")
	}
	// The parsed catalog feeds Plan unchanged: a LiteLLM price
	// applies exactly like a models.dev price.
	plan := Plan(cat, []LocalProvider{{ID: "openai", CatalogID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}},
		func(_, _ string) bool { return false }, nil)
	if len(plan.Updates) != 1 || plan.Updates[0].Price == nil || *plan.Updates[0].Price.InputUSDPer1M != 2.5 {
		t.Fatalf("Plan over LiteLLM catalog = %+v, want one update with input 2.5", plan.Updates)
	}
}
