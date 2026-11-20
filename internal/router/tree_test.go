package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// treeHook is a deterministic StrategyHook for tree tests: it
// orders attempts by a per-strategy cost/latency table instead
// of live signals, so the tree evaluation itself — not the
// scorer — is what the test proves.
func treeHook(costs, latencies map[string]float64) func(string) func([]Attempt) []Attempt {
	return func(strategy string) func([]Attempt) []Attempt {
		return func(chain []Attempt) []Attempt {
			out := append([]Attempt(nil), chain...)
			key := costs
			if strategy == "fastest" || strategy == "least-latency" {
				key = latencies
			}
			// Stable insertion sort: deterministic, no
			// dependence on sort package behaviour.
			for i := 1; i < len(out); i++ {
				for j := i; j > 0 && key[out[j].ProviderID] < key[out[j-1].ProviderID]; j-- {
					out[j], out[j-1] = out[j-1], out[j]
				}
			}
			return out
		}
	}
}

// treeTestEngine builds an engine whose strategy hook ranks by
// the test tables: costs a=10 b=1 c=5 d=7, latencies a=5 b=50
// c=10 d=20.
func treeTestEngine(t *testing.T, clients map[string]provider.Client) *Engine {
	t.Helper()
	costs := map[string]float64{"a": 10, "b": 1, "c": 5, "d": 7}
	latencies := map[string]float64{"a": 5, "b": 50, "c": 10, "d": 20}
	cfg := testConfig()
	cfg.StrategyHook = treeHook(costs, latencies)
	return NewEngine(clients, nil, cfg)
}

func treeExample() *strategytree.Node {
	// cheapest-of[fastest-of[a, b], c]
	return &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
		{Strategy: "fastest", Children: []*strategytree.Node{
			{Provider: "a", Model: "m"},
			{Provider: "b", Model: "m"},
		}},
		{Provider: "c", Model: "m"},
	}}
}

func attemptIDs(chain []Attempt) []string {
	out := make([]string, 0, len(chain))
	for _, att := range chain {
		out = append(out, att.ProviderID)
	}
	return out
}

func TestOrderAttemptsByTreeNested(t *testing.T) {
	eng := treeTestEngine(t, map[string]provider.Client{})
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	ordered, skipped, err := eng.orderAttemptsByTree(chain, treeExample(), "", "hard")
	if err != nil {
		t.Fatalf("orderAttemptsByTree = %v, want nil", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("zeroSkipped = %v, want none", skipped)
	}
	// fastest-of[a,b] picks a (latency 5 < 50); cheapest then
	// compares a (cost 10) with c (cost 5) and picks c first,
	// so the order is c, then a's whole subtree (a, b).
	if got := strings.Join(attemptIDs(ordered), ","); got != "c,a,b" {
		t.Fatalf("ordered = %s, want c,a,b (cheapest-of[fastest-of[a,b], c])", got)
	}
}

func TestOrderAttemptsByTreeUnmatchedAppended(t *testing.T) {
	eng := treeTestEngine(t, map[string]provider.Client{})
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "d", Model: "m"}, {ProviderID: "c", Model: "m"}}
	ordered, _, err := eng.orderAttemptsByTree(chain, treeExample(), "", "hard")
	if err != nil {
		t.Fatalf("orderAttemptsByTree = %v, want nil", err)
	}
	// d is named by no leaf: it must still be tried, after the
	// tree-ordered attempts, in chain order — never dropped.
	if got := strings.Join(attemptIDs(ordered), ","); got != "c,a,d" {
		t.Fatalf("ordered = %s, want c,a,d (unmatched d appended last)", got)
	}
}

func TestOrderAttemptsByTreeRootOverride(t *testing.T) {
	eng := treeTestEngine(t, map[string]provider.Client{})
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	// A per-request override (contract sort / auto:<axis> alias)
	// replaces the ROOT strategy only: fastest-of[
	// fastest-of[a,b], c] ranks a (5) over c (10) first.
	ordered, _, err := eng.orderAttemptsByTree(chain, treeExample(), "fastest", "hard")
	if err != nil {
		t.Fatalf("orderAttemptsByTree = %v, want nil", err)
	}
	if got := strings.Join(attemptIDs(ordered), ","); got != "a,b,c" {
		t.Fatalf("ordered = %s, want a,b,c (root override fastest)", got)
	}
	// The inner node keeps its own strategy under the override:
	// with root "cheapest" the inner fastest still picks a over
	// b (see TestOrderAttemptsByTreeNested); with a root override
	// of "cheapest" again the result is unchanged.
	ordered, _, err = eng.orderAttemptsByTree(chain, treeExample(), "cheapest", "hard")
	if err != nil {
		t.Fatalf("orderAttemptsByTree = %v, want nil", err)
	}
	if got := strings.Join(attemptIDs(ordered), ","); got != "c,a,b" {
		t.Fatalf("ordered = %s, want c,a,b (root override cheapest)", got)
	}
}

func TestOrderAttemptsByTreeRejects(t *testing.T) {
	eng := treeTestEngine(t, map[string]provider.Client{})
	chain := []Attempt{{ProviderID: "a", Model: "m"}}
	cases := []struct {
		name string
		tree *strategytree.Node
		want string
	}{
		{"unknown strategy", &strategytree.Node{Strategy: "weird", Children: []*strategytree.Node{{Provider: "a"}}}, "unknown strategy"},
		{"unknown strategy nested", &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
			{Strategy: "nope", Children: []*strategytree.Node{{Provider: "a"}}},
		}}, "unknown strategy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := eng.orderAttemptsByTree(chain, tc.tree, "", "hard")
			if err == nil {
				t.Fatalf("orderAttemptsByTree = nil error, want one containing %q", tc.want)
			}
			if !errors.Is(err, ErrInvalidStrategyTree) {
				t.Fatalf("error = %v, want it to wrap ErrInvalidStrategyTree", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestOrderAttemptsByTreeCycleAndDepthDoNotHang(t *testing.T) {
	eng := treeTestEngine(t, map[string]provider.Client{})
	chain := []Attempt{{ProviderID: "a", Model: "m"}}
	// A self-referential tree: evaluation must return an error,
	// never recurse until the stack overflows.
	cyclic := &strategytree.Node{Strategy: "balanced"}
	cyclic.Children = []*strategytree.Node{cyclic}
	done := make(chan error, 1)
	go func() {
		_, _, err := eng.orderAttemptsByTree(chain, cyclic, "", "hard")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("cyclic tree evaluated without error")
		}
		if !errors.Is(err, ErrInvalidStrategyTree) {
			t.Fatalf("cyclic tree error = %v, want ErrInvalidStrategyTree", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("cyclic tree evaluation did not return within 5s (stack overflow / unbounded recursion?)")
	}
	// A tree nested past MaxDepth: same — an error, not a crash.
	deep := &strategytree.Node{Strategy: "balanced"}
	cur := deep
	for i := 0; i < strategytree.MaxDepth; i++ {
		next := &strategytree.Node{Strategy: "balanced"}
		cur.Children = []*strategytree.Node{next}
		cur = next
	}
	cur.Children = []*strategytree.Node{{Provider: "a"}}
	if _, _, err := eng.orderAttemptsByTree(chain, deep, "", "hard"); !errors.Is(err, ErrInvalidStrategyTree) {
		t.Fatalf("over-deep tree error = %v, want ErrInvalidStrategyTree", err)
	}
}

func TestStrategyTreeForPrecedence(t *testing.T) {
	global := &strategytree.Node{Provider: "g"}
	perChain := &strategytree.Node{Provider: "p"}
	cfg := testConfig()
	cfg.StrategyTree = global
	cfg.ChainStrategyTree = func(model string) *strategytree.Node {
		if model == "special" {
			return perChain
		}
		return nil
	}
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	if got := eng.strategyTreeFor("special"); got != perChain {
		t.Fatalf("strategyTreeFor(special) = %v, want the per-chain tree", got)
	}
	if got := eng.strategyTreeFor("other"); got != global {
		t.Fatalf("strategyTreeFor(other) = %v, want the global tree", got)
	}
	cfg2 := testConfig()
	eng2 := NewEngine(map[string]provider.Client{}, nil, cfg2)
	if got := eng2.strategyTreeFor("special"); got != nil {
		t.Fatalf("strategyTreeFor with no trees = %v, want nil", got)
	}
}

func TestTreeRootStrategy(t *testing.T) {
	global := &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{{Provider: "a"}}}
	cfg := testConfig()
	cfg.StrategyTree = global
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	if _, root, ok := eng.treeRootStrategy("m", "", false); !ok || root != "cheapest" {
		t.Fatalf("treeRootStrategy(no override) = (%q, %v), want (cheapest, true)", root, ok)
	}
	if _, root, ok := eng.treeRootStrategy("m", "fastest", false); !ok || root != "fastest" {
		t.Fatalf("treeRootStrategy(override) = (%q, %v), want (fastest, true)", root, ok)
	}
	if _, _, ok := eng.treeRootStrategy("m", "", true); ok {
		t.Fatalf("treeRootStrategy(contract order) = ok, want false (order skips the tree)")
	}
	// A root with no strategy reads as priority (M1.11 default).
	bare := &strategytree.Node{Children: []*strategytree.Node{{Provider: "a"}}}
	cfg.StrategyTree = bare
	eng = NewEngine(map[string]provider.Client{}, nil, cfg)
	if _, root, ok := eng.treeRootStrategy("m", "", false); !ok || root != "priority" {
		t.Fatalf("treeRootStrategy(bare root) = (%q, %v), want (priority, true)", root, ok)
	}
}

// TestChatWithChainStrategyTreeServesLeafAndTrace drives a real
// request through the tree: cheapest-of[fastest-of[a,b], c]
// serves c first (cheapest winner), and the trace carries the
// tree path to the leaf that served it.
func TestChatWithChainStrategyTreeServesLeafAndTrace(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	c := okClient("from-c")
	eng2 := treeTestEngine(t, map[string]provider.Client{"a": a, "b": b, "c": c})
	eng2.cfg.StrategyTree = treeExample()
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	resp, trace, err := eng2.ChatWithChainStrategy(context.Background(), chain, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v, want nil", err)
	}
	if resp.Content != "from-c" {
		t.Fatalf("response = %+v, want content from-c (cheapest winner serves first)", resp)
	}
	if c.chatCalls() != 1 || a.chatCalls() != 0 || b.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=0 b=0 c=1", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	if trace == nil {
		t.Fatalf("trace is nil")
	}
	if got := strings.Join(trace.StrategyTreePath, " > "); got != "cheapest > c" {
		t.Fatalf("trace.StrategyTreePath = %v, want [cheapest c]", trace.StrategyTreePath)
	}
}

// TestChatWithChainStrategyTreeContractOrderWins proves the
// contract composition: an explicit routing.order skips the tree
// entirely, so the client's stated order is what is tried.
func TestChatWithChainStrategyTreeContractOrderWins(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	c := okClient("from-c")
	eng := treeTestEngine(t, map[string]provider.Client{"a": a, "b": b, "c": c})
	eng.cfg.StrategyTree = treeExample()
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	req.Routing = &provider.RequestRouting{Order: []string{"b", "a", "c"}}
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v, want nil", err)
	}
	if resp.Content != "from-b" {
		t.Fatalf("response = %+v, want content from-b (contract order first)", resp)
	}
	if b.chatCalls() != 1 || a.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d, want a=0 b=1 (tree must not re-rank the order)", a.chatCalls(), b.chatCalls())
	}
	if len(trace.StrategyTreePath) != 0 {
		t.Fatalf("trace.StrategyTreePath = %v, want empty (the tree did not order this request)", trace.StrategyTreePath)
	}
}

// TestChatWithChainStrategyTreeContractSortOverridesRoot proves
// the sort composition: routing.sort=latency replaces the tree
// root's strategy (cheapest -> least-latency) while the tree
// structure still applies — fastest leaf a serves first, and
// the trace path names least-latency at the root.
func TestChatWithChainStrategyTreeContractSortOverridesRoot(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	c := okClient("from-c")
	eng := treeTestEngine(t, map[string]provider.Client{"a": a, "b": b, "c": c})
	eng.cfg.StrategyTree = treeExample()
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}
	req.Routing = &provider.RequestRouting{Sort: "latency"}
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, req, "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v, want nil", err)
	}
	if resp.Content != "from-a" {
		t.Fatalf("response = %+v, want content from-a (sort=latency root: least-latency picks a, latency 5)", resp)
	}
	if got := strings.Join(trace.StrategyTreePath, " > "); got != "least-latency > fastest > a" {
		t.Fatalf("trace.StrategyTreePath = %v, want [least-latency fastest a]", trace.StrategyTreePath)
	}
	if trace.Contract == nil || trace.Contract.Strategy != "least-latency" {
		t.Fatalf("trace.Contract.Strategy = %v, want least-latency", trace.Contract)
	}
}

// TestChatWithChainStrategyTreeFailoverWalksTreeOrder: the tree
// winner fails (429), so the request falls over to the next
// attempt in tree order inside the same request.
func TestChatWithChainStrategyTreeFailoverWalksTreeOrder(t *testing.T) {
	a := okClient("from-a")
	b := okClient("from-b")
	c := errClient(&provider.RateLimitError{})
	eng := treeTestEngine(t, map[string]provider.Client{"a": a, "b": b, "c": c})
	eng.cfg.StrategyTree = treeExample()
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	resp, trace, err := eng.ChatWithChainStrategy(context.Background(), chain, provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "hi"}}}, "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy = %v, want nil (failover inside the request)", err)
	}
	// Tree order is c, a, b: c 429s, a serves.
	if resp.Content != "from-a" {
		t.Fatalf("response = %+v, want content from-a (c failed, a is next in tree order)", resp)
	}
	if c.chatCalls() != 1 || a.chatCalls() != 1 || b.chatCalls() != 0 {
		t.Fatalf("calls a=%d b=%d c=%d, want a=1 b=0 c=1", a.chatCalls(), b.chatCalls(), c.chatCalls())
	}
	if trace == nil || len(trace.Attempts) < 2 {
		t.Fatalf("trace = %+v, want at least the failed c attempt and the serving a attempt", trace)
	}
}

// M1.11 acceptance: a tree node with no strategy — and a tree
// root that names none — defaults to priority (declared order),
// not to a re-ranking. The recording hook captures the strategy
// name the engine actually passes at the root.
func TestOrderAttemptsByTreeDefaultStrategyIsPriority(t *testing.T) {
	var seen []string
	recording := func(strategy string) func([]Attempt) []Attempt {
		seen = append(seen, strategy)
		return func(chain []Attempt) []Attempt { return chain }
	}
	cfg := testConfig()
	cfg.StrategyHook = recording
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	// Flat tree: root names no strategy, leaves in declared order.
	tree := &strategytree.Node{Children: []*strategytree.Node{
		{Provider: "a", Model: "m"},
		{Provider: "b", Model: "m"},
		{Provider: "c", Model: "m"},
	}}
	chain := []Attempt{{ProviderID: "a", Model: "m"}, {ProviderID: "b", Model: "m"}, {ProviderID: "c", Model: "m"}}
	ordered, _, err := eng.orderAttemptsByTree(chain, tree, "", "hard")
	if err != nil {
		t.Fatalf("orderAttemptsByTree = %v, want nil", err)
	}
	if got := strings.Join(attemptIDs(ordered), ","); got != "a,b,c" {
		t.Fatalf("ordered = %s, want a,b,c (declared order under the priority default)", got)
	}
	if len(seen) == 0 || seen[0] != "priority" {
		t.Fatalf("root strategy passed to hook = %v; want [priority ...] (empty strategy must default to priority)", seen)
	}
}

// M1.11 acceptance: treeRootStrategy names "priority" when the
// tree's root names no strategy.
func TestTreeRootStrategyDefaultIsPriority(t *testing.T) {
	cfg := testConfig()
	cfg.StrategyTree = &strategytree.Node{Children: []*strategytree.Node{
		{Provider: "a", Model: "m"},
	}}
	eng := NewEngine(map[string]provider.Client{}, nil, cfg)
	_, root, ok := eng.treeRootStrategy("m", "", false)
	if !ok {
		t.Fatal("treeRootStrategy reported no tree; want one (StrategyTree is set)")
	}
	if root != "priority" {
		t.Fatalf("treeRootStrategy = %q; want priority (empty root strategy defaults to priority)", root)
	}
}
