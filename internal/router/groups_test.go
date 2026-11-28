// User-named routing groups (CHECKLIST 173): a group-addressed
// request resolves to its members in order and flows through the
// engine exactly like any other chain — same failover, same ledger
// rows, same contract filters, same streaming path — while an
// unknown group (or a group that serves nothing for the model)
// fails honestly with a 400 naming it, before any provider is
// dialled.
package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// groupTestEngine builds an engine with three fake providers (a, b,
// c) all serving model "m", a default chain of all three in order,
// and a GroupChain hook serving groups "fast" = [a, b] and "slow" =
// [c]. groupCalls records every GroupChain invocation.
func groupTestEngine(t *testing.T) (*Engine, *fakeClient, *fakeClient, *fakeClient, *fakeLedger) {
	t.Helper()
	a := &fakeClient{}
	b := &fakeClient{}
	c := &fakeClient{}
	led := &fakeLedger{}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}, {ProviderID: "c", Model: model}}
	}
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		switch group {
		case "fast":
			return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}, true
		case "slow":
			return []Attempt{{ProviderID: "c", Model: model}}, true
		case "empty":
			return []Attempt{}, true
		default:
			return nil, false
		}
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, led, cfg)
	return eng, a, b, c, led
}

func TestParseGroupAlias(t *testing.T) {
	cases := []struct {
		model    string
		group    string
		concrete string
		isAlias  bool
	}{
		{"group:fast/m", "fast", "m", true},
		{"group:fast/meta-llama/llama-3", "fast", "meta-llama/llama-3", true},
		{"GROUP:Fast/m", "Fast", "m", true},
		{"group:fast", "", "", true},      // malformed: no model
		{"group:/m", "", "m", true},       // malformed: no group
		{"group:fast/", "fast", "", true}, // malformed: no model
		{"gpt-4o", "", "", false},         // ordinary model
		{"auto/fast", "", "", false},      // virtual model, not a group
		{"groupish/m", "", "", false},     // prefix must be exactly "group:"
		{"", "", "", false},
	}
	for _, tc := range cases {
		group, concrete, ok := parseGroupAlias(tc.model)
		if ok != tc.isAlias || group != tc.group || concrete != tc.concrete {
			t.Errorf("parseGroupAlias(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.model, group, concrete, ok, tc.group, tc.concrete, tc.isAlias)
		}
	}
}

// TestGroupResolutionOrder: routing.group "fast" routes through a
// then b in config order, failing over inside the group when a
// 429s — and never touches c, which is outside the group.
func TestGroupResolutionOrder(t *testing.T) {
	eng, a, b, c, led := groupTestEngine(t)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{}
	}
	b.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if req.Model != "m" {
			t.Errorf("B got model %q, want %q", req.Model, "m")
		}
		return &provider.ChatResponse{Content: "from-b"}, nil
	}
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast"}
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("Content = %q, want %q", resp.Content, "from-b")
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 || c.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=1 b=1 c=0", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2", tr)
	}
	if tr.Attempts[0].Provider != "a" || tr.Attempts[1].Provider != "b" {
		t.Errorf("trace order = %q, %q, want a, b", tr.Attempts[0].Provider, tr.Attempts[1].Provider)
	}
	if tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Errorf("trace contract group = %v, want fast", tr.Contract)
	}
	// Ledger: one failed row for a, one success row for b — the
	// group participates in usage accounting exactly like a chain.
	if led.count() != 2 {
		t.Fatalf("ledger rows = %d, want 2", led.count())
	}
	led.mu.Lock()
	defer led.mu.Unlock()
	if led.calls[0].provider != "a" || led.calls[0].ok {
		t.Errorf("ledger[0] = %+v, want failed row for a", led.calls[0])
	}
	if led.calls[1].provider != "b" || !led.calls[1].ok {
		t.Errorf("ledger[1] = %+v, want success row for b", led.calls[1])
	}
}

// TestGroupAliasForm: the "group:<name>/<model>" model alias
// addresses the group and rewrites the model, and the trace
// contract still names the group.
func TestGroupAliasForm(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if req.Model != "m" {
			t.Errorf("A got model %q, want %q", req.Model, "m")
		}
		return &provider.ChatResponse{Content: "from-a"}, nil
	}
	req := testReq()
	req.Model = "group:fast/m"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-a" {
		t.Fatalf("Content = %q, want %q", resp.Content, "from-a")
	}
	if a.chatCalls() != 1 || b.chatCalls() != 0 || c.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=1 b=0 c=0", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	tr := eng.LastTrace()
	if tr == nil || tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Errorf("trace contract group = %v, want fast", tr)
	}
}

// TestGroupAliasSlashModel: only the first slash splits, so a
// model ID containing "/" survives the alias intact.
func TestGroupAliasSlashModel(t *testing.T) {
	eng, a, _, _, _ := groupTestEngine(t)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		if req.Model != "meta-llama/llama-3" {
			t.Errorf("A got model %q, want %q", req.Model, "meta-llama/llama-3")
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	req := testReq()
	req.Model = "group:fast/meta-llama/llama-3"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

// TestGroupStream: failover inside a group works on the streaming
// path too — a 429 on a pre-commit stream fails over to b.
func TestGroupStream(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	a.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{}
	}
	b.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if err := onDelta("from-b"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "from-b"}, nil
	}
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast"}
	var got string
	resp, err := eng.ChatStream(context.Background(), req, func(s string) error { got += s; return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Content != "from-b" || got != "from-b" {
		t.Fatalf("resp=%q streamed=%q, want from-b", resp.Content, got)
	}
	if a.streamCalls() != 1 || b.streamCalls() != 1 || c.streamCalls() != 0 {
		t.Fatalf("streams a=%d b=%d c=%d, want a=1 b=1 c=0", a.streamCalls(), b.streamCalls(), c.streamCalls())
	}
}

// TestGroupContractFilters: the per-request contract filters the
// group chain — routing.only [b] leaves only b, and the trace
// records both the group and the exclusion.
func TestGroupContractFilters(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast", Only: []string{"b"}}
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	if a.chatCalls() != 0 || b.chatCalls() != 1 || c.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=0 b=1 c=0", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	tr := eng.LastTrace()
	if tr == nil || tr.Contract == nil || tr.Contract.Group != "fast" {
		t.Fatalf("trace contract group = %v, want fast", tr)
	}
	if len(tr.Contract.Excluded) != 1 || tr.Contract.Excluded[0].Provider != "a" || tr.Contract.Excluded[0].Reason != "not_in_only" {
		t.Errorf("excluded = %+v, want [{a not_in_only}]", tr.Contract.Excluded)
	}
}

func TestGroupUnknown(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "nope"}
	_, err := eng.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat accepted unknown group")
	}
	var ce *ContractError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T %v, want *ContractError", err, err)
	}
	if ce.Status != 400 {
		t.Errorf("status = %d, want 400", ce.Status)
	}
	if !strings.Contains(ce.Msg, `"nope"`) {
		t.Errorf("error %q does not name the group", ce.Msg)
	}
	if a.chatCalls() != 0 || b.chatCalls() != 0 || c.chatCalls() != 0 {
		t.Errorf("unknown group dialled a provider (a=%d b=%d c=%d)", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	// The rejection publishes a trace with the group and the
	// error, no attempts — the dashboard shows why, not a stale
	// trace.
	tr := eng.LastTrace()
	if tr == nil || tr.Contract == nil || tr.Contract.Group != "nope" || tr.Contract.Error == "" {
		t.Errorf("trace = %+v, want contract{group=nope, error set}", tr)
	}
	if tr != nil && len(tr.Attempts) != 0 {
		t.Errorf("trace has %d attempts, want 0", len(tr.Attempts))
	}
	// The same rejection on the streaming path, before any SSE.
	_, err = eng.ChatStream(context.Background(), req, func(string) error { return nil })
	if err == nil {
		t.Fatal("ChatStream accepted unknown group")
	}
	if !errors.As(err, &ce) || ce.Status != 400 || !strings.Contains(ce.Msg, `"nope"`) {
		t.Errorf("ChatStream err = %v, want 400 naming \"nope\"", err)
	}
}

// TestGroupUnknownNoHook: with no GroupChain wired at all, any
// group a request names is unknown — the engine never invents a
// group.
func TestGroupUnknownNoHook(t *testing.T) {
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": &fakeClient{}}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast"}
	_, err := eng.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat accepted a group with no groups configured")
	}
	var ce *ContractError
	if !errors.As(err, &ce) || ce.Status != 400 || !strings.Contains(ce.Msg, "no groups are configured") {
		t.Errorf("err = %v, want 400 naming no-groups-configured", err)
	}
}

// TestGroupAliasErrors: malformed aliases and a routing.group /
// alias conflict are all honest 400s naming the problem.
func TestGroupAliasErrors(t *testing.T) {
	eng, _, _, _, _ := groupTestEngine(t)
	cases := []struct {
		name  string
		model string
		group string
		want  string
	}{
		{"no model in alias", "group:fast", "", "no model"},
		{"empty group in alias", "group:/m", "", "group name is empty"},
		{"empty model in alias", "group:fast/", "", "no model"},
		{"conflict", "group:fast/m", "slow", "conflicts"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := testReq()
			req.Model = tc.model
			if tc.group != "" {
				req.Routing = &provider.RequestRouting{Group: tc.group}
			}
			_, err := eng.Chat(context.Background(), req)
			if err == nil {
				t.Fatalf("Chat accepted %q", tc.model)
			}
			var ce *ContractError
			if !errors.As(err, &ce) || ce.Status != 400 {
				t.Fatalf("err = %v, want 400 ContractError", err)
			}
			if !strings.Contains(ce.Msg, tc.want) {
				t.Errorf("error %q does not contain %q", ce.Msg, tc.want)
			}
		})
	}
}

// TestGroupEmptyExpansion: a group that exists but expands to no
// attempt for the model (no member serves it) fails with a 400
// naming the group and the model.
func TestGroupEmptyExpansion(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "empty"}
	_, err := eng.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("Chat accepted a group with no serving member")
	}
	var ce *ContractError
	if !errors.As(err, &ce) || ce.Status != 400 {
		t.Fatalf("err = %v, want 400 ContractError", err)
	}
	if !strings.Contains(ce.Msg, `"empty"`) || !strings.Contains(ce.Msg, `"m"`) {
		t.Errorf("error %q does not name the group and the model", ce.Msg)
	}
	if a.chatCalls() != 0 || b.chatCalls() != 0 || c.chatCalls() != 0 {
		t.Errorf("empty group dialled a provider (a=%d b=%d c=%d)", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
}

// TestGroupNoGroupUntouched: a request that names no group routes
// through the default chain exactly as before — group resolution
// never rewrites an ordinary request.
func TestGroupNoGroupUntouched(t *testing.T) {
	eng, a, b, c, _ := groupTestEngine(t)
	req := testReq()
	req.Model = "m"
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q, want ok", resp.Content)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 0 || c.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=1 b=0 c=0", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	if tr := eng.LastTrace(); tr != nil && tr.Contract != nil {
		t.Errorf("ordinary request carries a contract block: %+v", tr.Contract)
	}
}

// TestGroupOrderSurvivesStrategyHook: a strategy hook that reverses
// the chain must NOT re-rank a group-resolved chain — the operator's
// member order is final. This is the live-smoke failure in miniature:
// with the production balanced hook, a member that had just failed
// (a content refusal in the ledger) was demoted below the next
// member, so the group silently stopped trying its first member.
// The control (no group) proves the hook is live: the same engine
// reverses the ordinary chain and serves from c first.
func TestGroupOrderSurvivesStrategyHook(t *testing.T) {
	a := &fakeClient{}
	b := &fakeClient{}
	c := &fakeClient{}
	led := &fakeLedger{}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}, {ProviderID: "c", Model: model}}
	}
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group == "fast" {
			return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}}, true
		}
		return nil, false
	}
	cfg.StrategyHook = func(strategy string) func(chain []Attempt) []Attempt {
		return func(chain []Attempt) []Attempt {
			rev := make([]Attempt, len(chain))
			for i, att := range chain {
				rev[len(chain)-1-i] = att
			}
			return rev
		}
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, led, cfg)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{}
	}
	b.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-b"}, nil
	}
	c.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-c"}, nil
	}

	// Group request: a first (429), then b — despite the hook.
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "fast"}
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("group Chat: %v", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("group Content = %q, want from-b", resp.Content)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 || c.chatCalls() != 0 {
		t.Fatalf("group calls a=%d b=%d c=%d, want a=1 b=1 c=0", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 2 || tr.Attempts[0].Provider != "a" || tr.Attempts[1].Provider != "b" {
		t.Fatalf("group trace attempts = %v, want [a b]", tr)
	}

	// Control: no group — the hook reverses [a b c] to [c b a] and
	// c serves first.
	req2 := testReq()
	req2.Model = "m"
	resp2, err := eng.Chat(context.Background(), req2)
	if err != nil {
		t.Fatalf("plain Chat: %v", err)
	}
	if resp2.Content != "from-c" {
		t.Fatalf("plain Content = %q, want from-c (hook did not reorder)", resp2.Content)
	}
	tr2 := eng.LastTrace()
	if tr2 == nil || len(tr2.Attempts) == 0 || tr2.Attempts[0].Provider != "c" {
		t.Fatalf("plain trace attempts = %v, want first c", tr2)
	}
}

// ---------------------------------------------------------------------------
// Routing-group entries are (model, effort), not model alone
// (CHECKLIST 256): the same model at different reasoning efforts is
// a distinct cost/latency/quality point, so a group may hold it
// twice at different efforts, and the effort an entry carries is
// what the upstream actually sees for that attempt.

// TestGroupSameModelTwoEfforts: one group holding the same model
// twice — at low effort then at high effort — resolves to two
// distinct attempts. The first 429s, the second serves, and both
// the upstream and the trace see the effort each entry carried.
// The two entries use distinct keys (k1/k2): a hint-less 429 cools
// the KEY per :529F, so a second effort on the SAME key is skipped
// by the hard-mode loop-top skip — that is the per-key rate-limit
// model, not a group-resolution failure. The test's point is group
// resolution, so it exercises the realistic multi-key case.
func TestGroupSameModelTwoEfforts(t *testing.T) {
	a := &fakeClient{}
	var efforts []string
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		efforts = append(efforts, req.ReasoningEffort)
		if req.ReasoningEffort == "low" {
			return nil, &provider.RateLimitError{}
		}
		return &provider.ChatResponse{Content: "from-high"}, nil
	}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "efforts" {
			return nil, false
		}
		return []Attempt{{ProviderID: "a", Model: model, Effort: "low", KeyID: "k1"}, {ProviderID: "a", Model: model, Effort: "high", KeyID: "k2"}}, true
	}
	eng := NewEngine(map[string]provider.Client{ClientKey("a", "k1"): a, ClientKey("a", "k2"): a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "efforts"}
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-high" {
		t.Fatalf("Content = %q, want from-high", resp.Content)
	}
	if len(efforts) != 2 || efforts[0] != "low" || efforts[1] != "high" {
		t.Fatalf("upstream efforts = %v, want [low high] (two distinct entries, not one deduped model)", efforts)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2", tr)
	}
	if tr.Attempts[0].Effort != "low" || tr.Attempts[1].Effort != "high" {
		t.Errorf("trace efforts = %q, %q, want low, high", tr.Attempts[0].Effort, tr.Attempts[1].Effort)
	}
	if tr.Attempts[0].Provider != "a" || tr.Attempts[1].Provider != "a" {
		t.Errorf("trace providers = %q, %q, want a, a (same model, same provider, different effort)", tr.Attempts[0].Provider, tr.Attempts[1].Provider)
	}
}

// TestGroupSameModelTwoEffortsStream: the same (model, effort)
// pair is distinct on the streaming path too — the stream attempt
// carries its entry's effort and the trace records it.
func TestGroupSameModelTwoEffortsStream(t *testing.T) {
	a := &fakeClient{}
	a.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		if req.ReasoningEffort != "high" {
			t.Errorf("stream attempt effort = %q, want high", req.ReasoningEffort)
		}
		if err := onDelta("hi"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "hi"}, nil
	}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "efforts" {
			return nil, false
		}
		return []Attempt{{ProviderID: "a", Model: model, Effort: "high"}}, true
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "efforts"}
	var sb strings.Builder
	resp, err := eng.ChatStream(context.Background(), req, func(d string) error { sb.WriteString(d); return nil })
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.String() != "hi" || resp.Content != "hi" {
		t.Errorf("deltas = %q, content = %q, want hi, hi", sb.String(), resp.Content)
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 1 || tr.Attempts[0].Effort != "high" {
		t.Errorf("trace = %+v, want one attempt with effort high", tr)
	}
}

// TestGroupEffortOverridesClientEffort: a group entry's effort wins
// over the client's reasoning_effort for that attempt — the
// operator's routing decision is final for the entry.
func TestGroupEffortOverridesClientEffort(t *testing.T) {
	a := &fakeClient{}
	var seen string
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seen = req.ReasoningEffort
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "efforts" {
			return nil, false
		}
		return []Attempt{{ProviderID: "a", Model: model, Effort: "low"}}, true
	}
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "m"
	req.ReasoningEffort = "medium" // client asked medium
	req.Routing = &provider.RequestRouting{Group: "efforts"}
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if seen != "low" {
		t.Errorf("upstream effort = %q, want low (group entry overrides the client)", seen)
	}
}

// TestGroupNoEffortPassesClientEffort: an attempt with no entry
// effort passes the client's reasoning_effort through untouched —
// on the chat path and on the stream path.
func TestGroupNoEffortPassesClientEffort(t *testing.T) {
	a := &fakeClient{}
	var seenChat, seenStream string
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		seenChat = req.ReasoningEffort
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	a.streamFn = func(ctx context.Context, req provider.ChatRequest, onDelta func(string) error) (*provider.ChatResponse, error) {
		seenStream = req.ReasoningEffort
		if err := onDelta("ok"); err != nil {
			return nil, err
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt { return []Attempt{{ProviderID: "a", Model: model}} }
	eng := NewEngine(map[string]provider.Client{"a": a}, &fakeLedger{}, cfg)
	req := testReq()
	req.Model = "m"
	req.ReasoningEffort = "high"
	if _, err := eng.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if seenChat != "high" {
		t.Errorf("chat upstream effort = %q, want high (client value passed through)", seenChat)
	}
	var sb strings.Builder
	if _, err := eng.ChatStream(context.Background(), req, func(d string) error { sb.WriteString(d); return nil }); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if sb.String() != "ok" {
		t.Errorf("stream deltas = %q, want ok", sb.String())
	}
	if seenStream != "high" {
		t.Errorf("stream upstream effort = %q, want high (client value passed through)", seenStream)
	}
}

// TestNestedGroupRefExpandsThroughRealPath (Item 2): a routing group
// whose member is another "group:<name>" (a nested combo-ref) expands
// recursively to the inner group's providers, in order, through the
// real Chat path. Groups: outer = [a, group:mid], mid = [b, c], so the
// flattened chain is a, b, c. FAILS-WHEN-OFF: without recursive
// expansion "group:mid" would be an unresolvable member and the chain
// would not be a, b, c.
func TestNestedGroupRefExpandsThroughRealPath(t *testing.T) {
	a := &fakeClient{}
	b := &fakeClient{}
	c := &fakeClient{}
	led := &fakeLedger{}
	cfg := testConfig()
	cfg.ChainFor = func(model string) []Attempt {
		return []Attempt{{ProviderID: "a", Model: model}, {ProviderID: "b", Model: model}, {ProviderID: "c", Model: model}}
	}
	groups := map[string][]string{
		"outer": {"a", "group:mid"},
		"mid":   {"b", "c"},
	}
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		members, ok := config.ExpandGroupMembers(groups, group)
		if !ok {
			return nil, false
		}
		var chain []Attempt
		for _, m := range members {
			chain = append(chain, Attempt{ProviderID: m, Model: model})
		}
		return chain, true
	}
	eng := NewEngine(map[string]provider.Client{"a": a, "b": b, "c": c}, led, cfg)
	a.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.RateLimitError{}
	}
	b.chatFn = func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "from-b"}, nil
	}
	req := testReq()
	req.Model = "m"
	req.Routing = &provider.RequestRouting{Group: "outer"}
	resp, err := eng.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("Content = %q, want from-b", resp.Content)
	}
	if a.chatCalls() != 1 || b.chatCalls() != 1 || c.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=1 b=1 c=0 (nested mid expanded to b,c)", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	tr := eng.LastTrace()
	if tr == nil || len(tr.Attempts) != 2 {
		t.Fatalf("trace attempts = %v, want 2", tr)
	}
	if tr.Attempts[0].Provider != "a" || tr.Attempts[1].Provider != "b" {
		t.Errorf("trace order = %q, %q, want a, b (nested expansion in order)", tr.Attempts[0].Provider, tr.Attempts[1].Provider)
	}
}
