// A2-3b: the fusion LLM judge is fusion's DEFAULT (the first panel
// member) and the judge model + panel knobs are reachable from the
// config file.
//
// FAILS-WHEN-OFF: revert the combo.go change that drops the
// "e.cfg.JudgeModel != nil" gate and the judge never runs - the served
// content is a panel member's verbatim text (no synthesis) and the
// judge client is never dialled for the pick/synthesis prompts.
package router

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// defaultJudgeClient is a panel member that ALSO behaves like a judge
// model: as a panel member it answers with its panel text, as a judge
// it answers the winner-pick prompt with "WINNER: 1" and the synthesis
// prompt with the synthesised answer. It records every prompt so a test
// can prove the judge prompts really reached it.
type defaultJudgeClient struct {
	mu      sync.Mutex
	chats   int
	prompts []string
	panel   string
	synth   string
	comply  bool
}

func (c *defaultJudgeClient) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	var p strings.Builder
	for _, m := range req.Messages {
		p.WriteString(m.Content)
	}
	c.mu.Lock()
	c.chats++
	c.prompts = append(c.prompts, p.String())
	comply, synth, panel := c.comply, c.synth, c.panel
	c.mu.Unlock()
	switch {
	case strings.Contains(p.String(), "WINNER: <number>"):
		if !comply {
			return &provider.ChatResponse{Content: "I cannot decide"}, nil
		}
		return &provider.ChatResponse{Content: "WINNER: 1"}, nil
	case strings.Contains(p.String(), "Panel answers (anonymised)"):
		return &provider.ChatResponse{Content: synth}, nil
	default:
		return &provider.ChatResponse{Content: panel}, nil
	}
}

func (c *defaultJudgeClient) ChatStream(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
	return c.Chat(ctx, req)
}

func (c *defaultJudgeClient) ListModels(ctx context.Context) ([]string, error) { return nil, nil }
func (c *defaultJudgeClient) TestKey(ctx context.Context) error                { return nil }

func (c *defaultJudgeClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chats
}

func (c *defaultJudgeClient) sawPrompt(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.prompts {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

// TestFusionDefaultsToFirstPanelMemberAsJudge: with NO judge model
// configured, fusion still runs the real LLM judge - the first panel
// member is the judge (the fusion default) - and SYNTHESISES one answer
// from the panel. The served content is the synthesised text, not any
// panel member's verbatim answer.
func TestFusionDefaultsToFirstPanelMemberAsJudge(t *testing.T) {
	const synth = "SYNTHESISED: both panel members agree on X; Source 2 adds Y."
	jc := &defaultJudgeClient{panel: "panel answer from the first member", synth: synth, comply: true}
	b := okClient("panel answer from the second member")
	eng := NewEngine(map[string]provider.Client{"a": jc, "b": b}, nil, testConfig())

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != synth {
		t.Fatalf("fusion answer = %q, want the synthesised answer %q", resp.Content, synth)
	}
	if resp.Content == "panel answer from the first member" || resp.Content == "panel answer from the second member" {
		t.Fatalf("fusion answer = %q is a verbatim panel member, not a synthesis", resp.Content)
	}
	if !jc.sawPrompt("WINNER: <number>") {
		t.Fatalf("the judge never saw the winner-pick prompt; prompts were %v", jc.prompts)
	}
	if !jc.sawPrompt("Panel answers (anonymised)") {
		t.Fatalf("the judge never saw the anonymised synthesis prompt; prompts were %v", jc.prompts)
	}
	// The first panel member is dialled once as a panel member, once for
	// the winner pick and once for the synthesis; the second member once.
	if jc.calls() != 3 {
		t.Fatalf("first panel member calls = %d, want 3 (panel + winner pick + synthesis)", jc.calls())
	}
	if b.chatCalls() != 1 {
		t.Fatalf("second panel member calls = %d, want 1 (panel only)", b.chatCalls())
	}
}

// TestFusionDefaultJudgeFailureFallsBackToHeuristicVerbatim: the default
// judge (the first panel member) that cannot answer the winner-pick
// prompt makes the LLM judge fail. Fusion then falls back to the
// deterministic HeuristicJudge and serves the judged winner VERBATIM - a
// failed judge never synthesises and never fails the request.
func TestFusionDefaultJudgeFailureFallsBackToHeuristicVerbatim(t *testing.T) {
	jc := &defaultJudgeClient{panel: "short", comply: false}
	b := okClient("a much longer answer that the heuristic judge must pick")
	eng := NewEngine(map[string]provider.Client{"a": jc, "b": b}, nil, testConfig())

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "a much longer answer that the heuristic judge must pick" {
		t.Fatalf("answer = %q, want the heuristic winner verbatim (a failed judge never synthesises)", resp.Content)
	}
	// The panel member is dialled once for the panel and once for the
	// (failed) winner pick; there is no synthesis call.
	if jc.calls() != 2 {
		t.Fatalf("first panel member calls = %d, want 2 (panel + failed winner pick, no synthesis)", jc.calls())
	}
}

// TestFusionDefaultJudgeSingleSurvivorNoJudgeCall: a panel with a single
// survivor is served directly - the default judge is NOT called, so a
// one-answer panel never pays for a judge round-trip.
func TestFusionDefaultJudgeSingleSurvivorNoJudgeCall(t *testing.T) {
	jc := &defaultJudgeClient{panel: "the only survivor answer", comply: true}
	eng := NewEngine(map[string]provider.Client{
		"a": jc,
		"b": errClient(context.DeadlineExceeded),
	}, nil, testConfig())

	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), comboReq(), "fusion")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(fusion) = %v, want nil", err)
	}
	if resp.Content != "the only survivor answer" {
		t.Fatalf("answer = %q, want the single survivor verbatim", resp.Content)
	}
	if jc.calls() != 1 {
		t.Fatalf("first panel member calls = %d, want 1 (panel only - no judge call for a single survivor)", jc.calls())
	}
}
