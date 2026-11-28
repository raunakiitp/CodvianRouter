// A2-4: JEV LLM judge behind a feature flag.
//
// The JEV strategy's LLM-judge scoring normally uses the
// deterministic heuristicJevScorer. This file pins the feature flag
// (Config.ComboJevLLMJudge) that swaps in a real LLM judge call
// (llmJevScorer) to the configured judge model. Default OFF keeps
// the heuristic preference order with NO judge call; a judge failure
// (or no resolvable judge model) falls back to the heuristic scorer,
// so the request never fails because the judge did.
//
// FAILS-WHEN-OFF: TestJevLLMJudgeWhenFlagOn fails if the jev branch
// stops calling the judge (the served winner would be the heuristic
// pick, and judge.calls() would be 0). TestJevHeuristicWhenFlagOff
// fails if the flag is forced on (the judge would be called).
package router

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

const (
	// jevPanelShort / jevPanelLong are the panel fixtures: the judge
	// picks the short answer, the heuristic scorer prefers the long one.
	jevPanelShort = "short"
	jevPanelLong  = "a much longer panel answer that the heuristic judge would prefer over a"
)

// jevJudgeClient is a fake judge model for the JEV scorer prompt: it
// records every chat so a test can prove the judge call happened, and
// answers with reply (a literal "WINNER: .. CONFIDENCE: .. PROBS: .."
// line) or failErr when set.
type jevJudgeClient struct {
	mu      sync.Mutex
	chats   int
	reply   string
	failErr error
}

func (c *jevJudgeClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	c.mu.Lock()
	c.chats++
	fail := c.failErr
	reply := c.reply
	c.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	return &provider.ChatResponse{Content: reply}, nil
}

func (c *jevJudgeClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return c.Chat(ctx, req)
}

func (c *jevJudgeClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *jevJudgeClient) TestKey(ctx context.Context) error                { return nil }

func (c *jevJudgeClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats
}

// jevJudgeEngine builds an engine with a jev panel of [a,b] and a
// dedicated judge provider, so the judge model is NOT a panel member.
func jevJudgeEngine(t *testing.T, judge *jevJudgeClient, enabled bool) *Engine {
	t.Helper()
	cfg := testConfig()
	cfg.ComboJevLLMJudge = enabled
	cfg.JevJudgeModel = func(model string) string { return "judge-model" }
	cfg.ChainFor = func(model string) []Attempt {
		if model == "judge-model" {
			return []Attempt{{ProviderID: "judge", Model: model}}
		}
		return nil
	}
	clients := map[string]provider.Client{
		"a":     okClient(jevPanelShort),
		"b":     okClient(jevPanelLong),
		"judge": judge,
	}
	return NewEngine(clients, nil, cfg)
}

// TestJevLLMJudgeWhenFlagOn: with ComboJevLLMJudge on and a judge
// model configured, the JEV path calls the judge exactly once and
// serves the JUDGE'S choice — the judge picks candidate 1 ("a", the
// short answer), which the heuristic scorer would never pick.
// FAILS-WHEN-OFF: if the jev branch stops wiring the LLM judge, the
// judge is never called (calls()==0) and the served winner is the
// heuristic pick ("b"), so both assertions fail.
func TestJevLLMJudgeWhenFlagOn(t *testing.T) {
	judge := &jevJudgeClient{reply: "WINNER: 1 CONFIDENCE: 0.8 PROBS: 0.8 0.2"}
	eng := jevJudgeEngine(t, judge, true)

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "jev")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(jev) = %v, want nil", err)
	}
	if judge.calls() != 1 {
		t.Fatalf("judge calls = %d, want 1 (the JEV judge call)", judge.calls())
	}
	if resp.Content != jevPanelShort {
		t.Fatalf("served content = %q, want the judge's choice (%q) — got the heuristic pick instead", resp.Content, jevPanelShort)
	}
}

// TestJevHeuristicWhenFlagOff: with ComboJevLLMJudge OFF — even
// though a judge model IS configured — the JEV path never calls the
// judge and serves the heuristic scorer's choice (the longer answer,
// "b"). FAILS-WHEN-ON: if the flag is forced on, the judge would be
// called (calls()==1) and the served winner would be "a".
func TestJevHeuristicWhenFlagOff(t *testing.T) {
	judge := &jevJudgeClient{reply: "WINNER: 1 CONFIDENCE: 0.8 PROBS: 0.8 0.2"}
	eng := jevJudgeEngine(t, judge, false)

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "jev")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(jev) = %v, want nil", err)
	}
	if judge.calls() != 0 {
		t.Fatalf("judge calls = %d, want 0 (flag off: no judge call)", judge.calls())
	}
	if resp.Content != jevPanelLong {
		t.Fatalf("served content = %q, want the heuristic winner verbatim (flag off)", resp.Content)
	}
}

// TestJevJudgeFailureFallsBackToHeuristic: when the flag is on but
// the judge call fails, the JEV path falls back to the heuristic
// scorer — the request succeeds with the heuristic winner and the
// judge is never trusted. FAILS-WHEN-OFF: with the fallback removed
// the request would fail (the scorer error propagates), not succeed.
func TestJevJudgeFailureFallsBackToHeuristic(t *testing.T) {
	judge := &jevJudgeClient{failErr: errors.New("judge model is down")}
	eng := jevJudgeEngine(t, judge, true)

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "jev")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(jev) with failing judge = %v, want nil (heuristic fallback)", err)
	}
	if resp.Content != jevPanelLong {
		t.Fatalf("served content = %q, want the heuristic winner (judge failed, so no judge choice)", resp.Content)
	}
}

// TestParseJevJudge: the parser reads choice + confidence +
// per-candidate probabilities and rejects a malformed reply.
func TestParseJevJudge(t *testing.T) {
	cands := []PanelResult{
		{Attempt: panelAtt("a", "m"), Text: "short"},
		{Attempt: panelAtt("b", "m"), Text: "long"},
	}
	answer, err := parseJevJudge("WINNER: 2 CONFIDENCE: 0.9 PROBS: 0.1, 0.9", cands)
	if err != nil {
		t.Fatalf("parseJevJudge: %v", err)
	}
	if answer.Choice != "b/m" {
		t.Fatalf("Choice = %q, want %q", answer.Choice, "b/m")
	}
	if answer.Confidence != 0.9 {
		t.Fatalf("Confidence = %v, want 0.9", answer.Confidence)
	}
	if answer.Probabilities["a/m"] != 0.1 || answer.Probabilities["b/m"] != 0.9 {
		t.Fatalf("Probabilities = %v, want a/m=0.1 b/m=0.9", answer.Probabilities)
	}
	if _, err := parseJevJudge("no judgement here", cands); err == nil {
		t.Fatal("parseJevJudge(malformed) = nil error, want an error")
	}
	if _, err := parseJevJudge("WINNER: 9 CONFIDENCE: 0.9 PROBS: 0.5 0.5", cands); err == nil {
		t.Fatal("parseJevJudge(out-of-range winner) = nil error, want an error")
	}
}
