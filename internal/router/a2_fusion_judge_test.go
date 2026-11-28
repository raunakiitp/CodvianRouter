// A2-3: fusion LLM judge + grounded synthesis, configurable judge model,
// heuristic fallback, single-survivor direct answer, and the panel knobs.
//
// FAILS-WHEN-OFF: the synthesis assertions fail if the fusion branch stops
// calling synthesizeFusion (resp.Content would be a panel member's verbatim
// text, not the judge's synthesised answer); the knob tests fail if the new
// PanelConfig fields stop being honoured.
package router

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// judgeClient is a fake judge model: it answers the winner-pick prompt with
// "WINNER: 1" and the synthesis prompt with synthText. It records the prompts
// it saw so a test can prove BOTH calls happened (pick + synthesis).
type judgeClient struct {
	mu      sync.Mutex
	chats   int
	prompts []string
	synth   string
}

func (c *judgeClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	var p strings.Builder
	for _, m := range req.Messages {
		p.WriteString(m.Content)
	}
	c.mu.Lock()
	c.chats++
	c.prompts = append(c.prompts, p.String())
	synth := c.synth
	c.mu.Unlock()
	if strings.Contains(p.String(), "WINNER: <number>") {
		return &provider.ChatResponse{Content: "WINNER: 1"}, nil
	}
	return &provider.ChatResponse{Content: synth}, nil
}

func (c *judgeClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return c.Chat(ctx, req)
}

func (c *judgeClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *judgeClient) TestKey(ctx context.Context) error                { return nil }

func (c *judgeClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats
}

func (c *judgeClient) sawSynthPrompt() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.prompts {
		if strings.Contains(p, "Panel answers (anonymised)") && strings.Contains(p, "Source 1:") {
			return true
		}
	}
	return false
}

// fusionJudgeEngine builds an engine with a fusion panel of [a,b] and a
// dedicated judge provider, so the judge model is NOT a panel member.
func fusionJudgeEngine(t *testing.T, judge *judgeClient) *Engine {
	t.Helper()
	cfg := testConfig()
	cfg.JudgeModel = func(model string) string { return "judge-model" }
	cfg.ChainFor = func(model string) []Attempt {
		if model == "judge-model" {
			return []Attempt{{ProviderID: "judge", Model: model}}
		}
		return nil
	}
	clients := map[string]provider.Client{
		"a":     okClient("short panel answer"),
		"b":     okClient("a much longer panel answer that the heuristic judge would prefer over a"),
		"judge": judge,
	}
	return NewEngine(clients, nil, cfg)
}

// TestFusionSynthesisesAnswerFromPanel (required A2-3 test): with a judge
// model configured, fusion runs the LLM judge and then SYNTHESISES one answer
// from the panel — the served content is the judge's synthesised text, NOT
// any panel member's verbatim answer, and the judge saw both the pick prompt
// and the anonymised "Source N" synthesis prompt.
func TestFusionSynthesisesAnswerFromPanel(t *testing.T) {
	const synth = "SYNTHESISED: the panel agrees on X and Y; Source 1 adds Z."
	judge := &judgeClient{synth: synth}
	eng := fusionJudgeEngine(t, judge)

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != synth {
		t.Fatalf("fusion answer = %q, want the synthesised answer %q (not a verbatim panel member)", resp.Content, synth)
	}
	if resp.Content == "short panel answer" || strings.HasPrefix(resp.Content, "a much longer panel answer") {
		t.Fatalf("fusion answer = %q, looks like a verbatim panel member, not a synthesis", resp.Content)
	}
	if judge.calls() != 2 {
		t.Fatalf("judge calls = %d, want 2 (one winner pick + one synthesis)", judge.calls())
	}
	if !judge.sawSynthPrompt() {
		t.Fatalf("judge never saw the anonymised synthesis prompt (Source N); prompts were %v", judge.prompts)
	}
}

// TestFusionJudgeFailureServesVerbatimHeuristic: a judge that names no winner
// fails the LLM judge; fusion falls back to the HeuristicJudge and serves the
// judged winner VERBATIM (no synthesis — a failed judge is never asked to
// synthesise).
func TestFusionJudgeFailureServesVerbatimHeuristic(t *testing.T) {
	// Force the judge to fail: override its Chat to never say WINNER.
	bad := &fakeClient{chatFn: func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "both are fine, hard to say"}, nil
	}}
	cfg := testConfig()
	cfg.JudgeModel = func(model string) string { return "judge-model" }
	cfg.ChainFor = func(model string) []Attempt {
		if model == "judge-model" {
			return []Attempt{{ProviderID: "judge", Model: model}}
		}
		return nil
	}
	eng := NewEngine(map[string]provider.Client{
		"a":     okClient("short"),
		"b":     okClient("the longest answer, which the heuristic judge prefers"),
		"judge": bad,
	}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "the longest answer, which the heuristic judge prefers" {
		t.Fatalf("answer = %q, want the heuristic winner verbatim (judge failed, so no synthesis)", resp.Content)
	}
	if bad.chatCalls() != 1 {
		t.Fatalf("judge calls = %d, want 1 (the failed pick; no synthesis after a judge failure)", bad.chatCalls())
	}
}

// TestFusionSingleSurvivorServedDirectly: when only one panel member succeeds,
// its answer is served directly with NO judge call and NO synthesis.
func TestFusionSingleSurvivorServedDirectly(t *testing.T) {
	judge := &judgeClient{synth: "never used"}
	cfg := testConfig()
	cfg.JudgeModel = func(model string) string { return "judge-model" }
	cfg.ChainFor = func(model string) []Attempt {
		if model == "judge-model" {
			return []Attempt{{ProviderID: "judge", Model: model}}
		}
		return nil
	}
	eng := NewEngine(map[string]provider.Client{
		"a":     errClient(errors.New("a is down")),
		"b":     okClient("the only survivor answer"),
		"judge": judge,
	}, nil, cfg)
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "the only survivor answer" {
		t.Fatalf("answer = %q, want the single survivor verbatim", resp.Content)
	}
	if judge.calls() != 0 {
		t.Fatalf("judge calls = %d, want 0 (a single survivor is served directly)", judge.calls())
	}
}

// TestFusionNoJudgeModelUsesPanelFirstMemberAsJudge: with no judge model
// configured the judge IS the first panel member (the fusion default,
// A2-3b) - so the panel member is dialled once for the panel and once more
// for the winner pick. This client never answers the pick prompt, so the
// LLM judge fails and fusion falls back to the deterministic HeuristicJudge
// and serves the judged winner VERBATIM (no synthesis). The default judge
// path with a working judge is covered by
// TestFusionDefaultsToFirstPanelMemberAsJudge (a2_fusion_default_judge_test.go).
func TestFusionNoJudgeModelUsesPanelFirstMemberAsJudge(t *testing.T) {
	a := okClient("short")
	b := okClient("a much longer answer that the heuristic judge must pick")
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b}, nil, testConfig())
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "a much longer answer that the heuristic judge must pick" {
		t.Fatalf("answer = %q, want the heuristic winner verbatim", resp.Content)
	}
	if a.chatCalls() != 2 || b.chatCalls() != 1 {
		t.Fatalf("panel calls a=%d b=%d, want a=2 (panel + failed winner pick as the default judge) and b=1 (panel only)", a.chatCalls(), b.chatCalls())
	}
}

// --- panel primitive knobs --------------------------------------------------

func TestPanelMinPanelServesDirectly(t *testing.T) {
	targets := []Attempt{panelAtt("p1", "m1"), panelAtt("p2", "m2")}
	call := fakePanelCaller(map[string]string{"m1": "first", "m2": "second"}, nil)
	res, err := RunFusion(context.Background(), "p", targets, call, FusionConfig{PanelConfig: PanelConfig{MinPanel: 3}})
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if res.JudgeName != "direct" || res.Winner.Attempt.Model != "m1" {
		t.Fatalf("result = %+v, want a direct answer (panel below MinPanel)", res)
	}
}

func TestPanelMaxPanelKnobRejects(t *testing.T) {
	targets := make([]Attempt, 3)
	for i := range targets {
		targets[i] = panelAtt("p", "m")
	}
	_, err := RunFusion(context.Background(), "p", targets, fakePanelCaller(nil, nil), FusionConfig{PanelConfig: PanelConfig{MaxPanel: 2}})
	if err == nil || !strings.Contains(err.Error(), "3") || !strings.Contains(err.Error(), "2") {
		t.Fatalf("err = %v, want it to name 3 targets over the max of 2", err)
	}
}

func TestPanelStragglerGraceAbandonsSlowMember(t *testing.T) {
	targets := []Attempt{panelAtt("fast", "m1"), panelAtt("slow", "m2")}
	call := func(ctx context.Context, att Attempt) (string, error) {
		if att.Model == "m2" {
			select {
			case <-time.After(2 * time.Second):
				return "late", nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "fast answer", nil
	}
	start := time.Now()
	res, err := RunFusion(context.Background(), "p", targets, call, FusionConfig{
		PanelConfig: PanelConfig{MinPanel: 1, StragglerGrace: 30 * time.Millisecond},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunFusion: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("RunFusion took %v, want it to return soon after the grace (not wait 2s for the straggler)", elapsed)
	}
	if len(res.Candidates) != 2 || res.Candidates[1].Err == nil {
		t.Fatalf("straggler candidate = %+v, want it recorded as an abandoned error", res.Candidates[1])
	}
	if !errors.Is(res.Candidates[1].Err, errStragglerAbandoned) {
		t.Fatalf("straggler err = %v, want errStragglerAbandoned", res.Candidates[1].Err)
	}
	if res.Winner.Text != "fast answer" {
		t.Fatalf("winner = %q, want the fast answer", res.Winner.Text)
	}
}

func TestPanelHardTimeoutKnobBoundsMember(t *testing.T) {
	targets := []Attempt{panelAtt("p1", "m1"), panelAtt("p2", "m2")}
	call := func(ctx context.Context, att Attempt) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	_, err := RunFusion(context.Background(), "p", targets, call, FusionConfig{PanelConfig: PanelConfig{Timeout: 50 * time.Millisecond}})
	if err == nil {
		t.Fatal("RunFusion with an always-blocking caller + hard timeout: got nil error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("RunFusion took %v, want the 50ms panel timeout to bound it", elapsed)
	}
}

// TestComboPanelConfigKnobs: the operator Config knobs map onto PanelConfig.
func TestComboPanelConfigKnobs(t *testing.T) {
	eng := NewEngine(nil, nil, testConfig())
	got := eng.comboPanelConfig()
	if got.MaxPanel != DefaultFusionMaxPanel {
		t.Fatalf("default MaxPanel = %d, want %d", got.MaxPanel, DefaultFusionMaxPanel)
	}
	cfg := testConfig()
	cfg.ComboMaxPanel = 7
	cfg.ComboMinPanel = 3
	cfg.ComboPanelHardTimeoutMs = 1234
	cfg.ComboStragglerGraceMs = 250
	eng2 := NewEngine(nil, nil, cfg)
	got2 := eng2.comboPanelConfig()
	if got2.MaxPanel != 7 || got2.MinPanel != 3 || got2.Timeout != 1234*time.Millisecond || got2.StragglerGrace != 250*time.Millisecond {
		t.Fatalf("comboPanelConfig = %+v, want MaxPanel 7 / MinPanel 3 / Timeout 1234ms / Grace 250ms", got2)
	}
}
