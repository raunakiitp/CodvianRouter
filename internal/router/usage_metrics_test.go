package router

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

type usageSinkCall struct {
	provider string
	model    string
	kind     string
	tokens   int
	usd      float64
}

type recordingUsageSink struct {
	tokens []usageSinkCall
	costs  []usageSinkCall
}

func (s *recordingUsageSink) IncUsageTokens(p, m, kind string, n int) {
	s.tokens = append(s.tokens, usageSinkCall{provider: p, model: m, kind: kind, tokens: n})
}
func (s *recordingUsageSink) AddReportedCost(p, m string, usd float64) {
	s.costs = append(s.costs, usageSinkCall{provider: p, model: m, usd: usd})
}

// T7: the engine's single accounting point forwards the provider's real
// token split and exact reported cost to the metrics sink. It must work
// without a ledger (metrics are independent of storage) and must be a
// no-op when no sink is wired — a nil sink must never panic or route
// differently. Because both chat and stream call recordAttempt, this
// one test covers both paths without fabricating stream responses.
func TestRecordAttemptFeedsUsageMetrics(t *testing.T) {
	eng := NewEngine(map[string]provider.Client{}, nil, Config{})
	sink := &recordingUsageSink{}
	eng.SetUsageMetrics(sink)
	cost := 0.012345
	eng.recordAttempt("key-1", Attempt{ProviderID: "p", Model: "m"}, true, 7, provider.Usage{
		PromptTokens: 100, CompletionTokens: 50, CachedTokens: 20,
		ReasoningTokens: 10, Cost: &cost,
	})
	if len(sink.tokens) != 4 || len(sink.costs) != 1 {
		t.Fatalf("got %d token calls and %d cost calls, want 4 and 1", len(sink.tokens), len(sink.costs))
	}
	wantKinds := map[string]int{"prompt": 100, "completion": 50, "cached": 20, "reasoning": 10}
	got := map[string]int{}
	for _, c := range sink.tokens {
		if c.provider != "p" || c.model != "m" {
			t.Errorf("wrong labels %q/%q", c.provider, c.model)
		}
		if _, dup := got[c.kind]; dup {
			t.Errorf("duplicate token kind %q", c.kind)
		}
		got[c.kind] = c.tokens
	}
	for kind, n := range wantKinds {
		if got[kind] != n {
			t.Errorf("kind %s = %d, want %d", kind, got[kind], n)
		}
	}
	if sink.costs[0].usd != cost || sink.costs[0].provider != "p" || sink.costs[0].model != "m" {
		t.Errorf("reported cost call = %+v, want p/m/%.6f", sink.costs[0], cost)
	}

	// No reported cost => no cost call; zero-token attempts emit no
	// fake series (the recorder discards zero/negative itself).
	sink2 := &recordingUsageSink{}
	eng.SetUsageMetrics(sink2)
	eng.recordAttempt("", Attempt{ProviderID: "p2", Model: "m2"}, false, 1, provider.Usage{})
	if len(sink2.costs) != 0 {
		t.Errorf("nil Cost must not fabricate a reported-cost sample: %+v", sink2.costs)
	}
	eng.SetUsageMetrics(nil)
	eng.recordAttempt("", Attempt{ProviderID: "p3", Model: "m3"}, true, 1, provider.Usage{PromptTokens: 1})
	if len(sink2.tokens) != 4 {
		t.Errorf("nil sink must be a no-op, got %d token calls", len(sink2.tokens))
	}
}
