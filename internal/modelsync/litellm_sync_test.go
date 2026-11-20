package modelsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// G16 Area 11 price coverage: the LiteLLM sync source. Parse only
// existed (TestParseLiteLLMCatalog); these tests prove the
// fetch/plan/apply path for the second source end to end against a
// local stand-in — prices apply for models missing them, never
// overwrite operator-set prices, deprecated models are skipped.
func TestG16LiteLLMSyncAppliesPrices(t *testing.T) {
	dir := t.TempDir()
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testLiteLLMJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, Source: SourceLiteLLM, URL: standin.URL}, filepath.Join(dir, StateFileName))
	if s.Source() != SourceLiteLLM {
		t.Fatalf("Source() = %q, want litellm", s.Source())
	}
	locals := []LocalProvider{{ID: "openai", CatalogID: "openai", Models: []LocalModel{
		{ID: "gpt-4o"},
		{ID: "gpt-4o-mini"},
		{ID: "gpt-3.5-turbo-0301"}, // deprecated in the past: skipped
	}}}
	noOperator := func(string, string) bool { return false }
	var gotPlan PlanResult
	apply := func(plan PlanResult) error { gotPlan = plan; return nil }
	counts, err := s.Sync(context.Background(), locals, noOperator, apply)
	if err != nil {
		t.Fatalf("LiteLLM Sync: %v", err)
	}
	if counts.PricesApplied != 2 {
		t.Fatalf("PricesApplied = %d, want 2 (gpt-4o + gpt-4o-mini; deprecated skipped)", counts.PricesApplied)
	}
	if counts.SkippedDeprecated != 1 {
		t.Fatalf("SkippedDeprecated = %d, want 1", counts.SkippedDeprecated)
	}
	if len(gotPlan.Updates) != 2 {
		t.Fatalf("updates = %d, want 2", len(gotPlan.Updates))
	}
	for _, u := range gotPlan.Updates {
		if u.Model == "gpt-4o" {
			if u.Price == nil || u.Price.InputUSDPer1M == nil || *u.Price.InputUSDPer1M != 2.5 {
				t.Fatalf("gpt-4o update price = %+v, want input 2.5 per 1M", u.Price)
			}
		}
	}
	// The synced layer holds the applied prices.
	if len(s.SyncedPrices()) != 2 {
		t.Fatalf("synced prices = %d, want 2", len(s.SyncedPrices()))
	}
}

func TestG16LiteLLMSyncNeverOverwritesOperatorPrice(t *testing.T) {
	dir := t.TempDir()
	standin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(testLiteLLMJSON))
	}))
	defer standin.Close()
	s := New(SyncerConfig{Enabled: true, Source: SourceLiteLLM, URL: standin.URL}, filepath.Join(dir, StateFileName))
	locals := []LocalProvider{{ID: "openai", CatalogID: "openai", Models: []LocalModel{{ID: "gpt-4o"}}}}
	// The operator priced gpt-4o: the sync must skip it.
	operatorHas := func(provider, model string) bool { return provider == "openai" && model == "gpt-4o" }
	counts, err := s.Sync(context.Background(), locals, operatorHas, nil)
	if err != nil {
		t.Fatalf("LiteLLM Sync: %v", err)
	}
	if counts.PricesApplied != 0 {
		t.Fatalf("PricesApplied = %d, want 0 (operator price wins)", counts.PricesApplied)
	}
	if counts.SkippedOperatorPrice != 1 {
		t.Fatalf("SkippedOperatorPrice = %d, want 1", counts.SkippedOperatorPrice)
	}
	if len(s.SyncedPrices()) != 0 {
		t.Fatalf("synced prices = %d, want 0", len(s.SyncedPrices()))
	}
}

func TestG16LiteLLMUnknownSourceFailsClosed(t *testing.T) {
	s := New(SyncerConfig{Enabled: true, Source: "no-such-source", URL: "http://127.0.0.1:1"}, "")
	if _, err := s.Sync(context.Background(), nil, nil, nil); err == nil {
		t.Fatal("unknown sync source synced without error")
	}
}

func TestG16LiteLLMDefaultURL(t *testing.T) {
	s := New(SyncerConfig{Enabled: true, Source: SourceLiteLLM}, "")
	if s.URL() != DefaultLiteLLMURL {
		t.Fatalf("URL() = %q, want DefaultLiteLLMURL", s.URL())
	}
	md := New(SyncerConfig{Enabled: true}, "")
	if md.URL() != DefaultURL || md.Source() != SourceModelsDev {
		t.Fatalf("default syncer URL/source = %q/%q, want models.dev defaults", md.URL(), md.Source())
	}
}
