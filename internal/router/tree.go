package router

import (
	"errors"
	"fmt"

	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
)

// Nestable strategy-tree evaluation (CHECKLIST 177).
//
// A configured tree orders a chain recursively: each internal
// node picks among its children by its own strategy, and the
// full ordered list of each child follows its winner. A leaf
// contributes the chain's attempts for its provider (and model,
// when the leaf pins one), in chain order.
//
// Composition, highest precedence first:
//
//   - A per-request routing contract with an explicit `order`
//     skips the tree entirely: the client's stated try order is
//     final (CHECKLIST 157), and the tree never re-ranks it.
//   - A per-request strategy override — the contract's `sort`
//     or an `auto:<axis>` alias — replaces the ROOT node's
//     strategy only: the tree structure still applies, with
//     the requested strategy at the top. Inner nodes keep
//     their own strategies.
//   - Otherwise the tree's own node strategies govern, with
//     the per-chain tree winning over the global tree for its
//     model. A contract that only filters (only / exclude /
//     max_price / data_collection) still lets the tree order
//     the filtered chain.
//
// A tree never drops a fallback target: attempts no leaf names
// are appended after the tree-ordered ones, in chain order, and
// an attempt named by two leaves is tried once (first
// occurrence wins). The zero-quota filter and the tail
// partitions (adaptive saturation, soft-demotion) apply exactly
// as on the flat path.

// ErrInvalidStrategyTree is returned when a strategy tree fails
// evaluation — a cycle, a depth overrun, or an unknown strategy
// on a node. Config load (config.Validate) already rejects such
// trees, so this can only fire for a tree supplied in-process
// past load; it is an honest request error, never a silent
// flatten and never a stack overflow.
var ErrInvalidStrategyTree = errors.New("router: invalid strategy tree")

// strategyTreeFor resolves the tree for one chain (model): the
// per-chain tree wins over the global tree; nil means no tree
// is configured for this model.
func (e *Engine) strategyTreeFor(model string) *strategytree.Node {
	if e.cfg.ChainStrategyTree != nil {
		if t := e.cfg.ChainStrategyTree(model); t != nil {
			return t
		}
	}
	return e.cfg.StrategyTree
}

// orderAttemptsByTree orders a chain by a strategy tree.
// rootOverride, when non-empty, replaces the root node's
// strategy (a per-request contract sort or auto:<axis> alias);
// "" means the tree's own node strategies govern. The returned
// error is ErrInvalidStrategyTree (wrapped with the node path)
// when the tree cannot be evaluated.
func (e *Engine) orderAttemptsByTree(chain []Attempt, tree *strategytree.Node, rootOverride, mode string) (ordered, zeroSkipped []Attempt, err error) {
	// Context-free entry point: the per-request hook (when
	// installed) runs with a zero context and its cacheability
	// gate stays closed — the same behaviour as before
	// M6.5-wiring. Request paths call
	// orderAttemptsByTreeWithContext.
	return e.orderAttemptsByTreeWithContext(chain, tree, rootOverride, mode, StrategyHookContext{})
}

// orderAttemptsByTreeWithContext is orderAttemptsByTree for one
// request: the same tree evaluation, with the request's
// StrategyHookContext threaded to the strategy hook at every
// internal node (rankAttempts), so a node's strategy means the
// same thing here as it does on a flat chain.
func (e *Engine) orderAttemptsByTreeWithContext(chain []Attempt, tree *strategytree.Node, rootOverride, mode string, hookCtx StrategyHookContext) (ordered, zeroSkipped []Attempt, err error) {
	eligible, zeroSkipped := e.filterZeroQuota(chain)
	evaluated, err := e.evalTree(tree, eligible, rootOverride, 1, make(map[*strategytree.Node]bool), hookCtx)
	if err != nil {
		return nil, zeroSkipped, err
	}
	// Attempts no leaf names, appended in chain order — a tree
	// never silently drops a fallback target.
	seen := make(map[Attempt]bool, len(evaluated))
	for _, att := range evaluated {
		seen[att] = true
	}
	for _, att := range eligible {
		if !seen[att] {
			evaluated = append(evaluated, att)
			seen[att] = true
		}
	}
	// Account-level fallback (M3 Area 1, PARITY row 388), same
	// as the flat path: within each (provider, model) group,
	// order the accounts under that provider's account fallback
	// strategy, before the reserve-last tail partition. The
	// tree path never runs under an explicit client order (the
	// caller skips the tree entirely then), so no preserveOrder
	// exemption is needed here.
	return e.applyTailPartitions(e.orderAccountAttempts(evaluated), mode), zeroSkipped, nil
}

// evalTree evaluates one subtree over the eligible attempts and
// returns them ordered by that subtree. depth counts nodes from
// the tree root (1); visiting tracks the nodes on the current
// path, so a cycle is an error instead of unbounded recursion.
func (e *Engine) evalTree(n *strategytree.Node, candidates []Attempt, rootOverride string, depth int, visiting map[*strategytree.Node]bool, hookCtx StrategyHookContext) ([]Attempt, error) {
	if n == nil {
		return nil, fmt.Errorf("%w: nil node", ErrInvalidStrategyTree)
	}
	if depth > strategytree.MaxDepth {
		return nil, fmt.Errorf("%w: node at depth %d exceeds the maximum depth of %d", ErrInvalidStrategyTree, depth, strategytree.MaxDepth)
	}
	if visiting[n] {
		return nil, fmt.Errorf("%w: cycle at node for provider %q", ErrInvalidStrategyTree, n.Provider)
	}
	visiting[n] = true
	defer delete(visiting, n)

	if len(n.Children) == 0 {
		// Leaf: the chain's attempts for this provider (and
		// model, when the leaf pins one), in chain order.
		var out []Attempt
		for _, att := range candidates {
			if att.ProviderID == n.Provider && (n.Model == "" || n.Model == att.Model) {
				out = append(out, att)
			}
		}
		return out, nil
	}

	// Internal node: evaluate every child over the same
	// candidate pool, then order the children by their
	// winners under this node's strategy.
	strategy := n.Strategy
	if depth == 1 && rootOverride != "" {
		strategy = rootOverride
	}
	if strategy == "" {
		strategy = "priority"
	}
	// "priority" (M1.11 default) and "code" (M1.10) are declared
	// strategies of internal/strategy, but strategytree's
	// allowlist does not list them yet — accept them here so a
	// defaulted or code-ordered node evaluates instead of
	// erroring. The canonical fix is to add both names to
	// strategytree.validStrategies / canonicalStrategies (and its
	// Validate error text); until then a tree node that names
	// "priority" or "code" explicitly in the config file is
	// still rejected at load by strategytree.Validate.
	if strategy != "priority" && strategy != "code" && !strategytree.IsValidStrategy(strategy) {
		return nil, fmt.Errorf("%w: unknown strategy %q", ErrInvalidStrategyTree, strategy)
	}
	childOrders := make([][]Attempt, 0, len(n.Children))
	for _, c := range n.Children {
		ord, err := e.evalTree(c, candidates, "", depth+1, visiting, hookCtx)
		if err != nil {
			return nil, err
		}
		childOrders = append(childOrders, ord)
	}
	// The winner of each non-empty child, ranked by this
	// node's strategy hook (the same hook the flat path
	// uses, so a node's strategy means the same thing here
	// as it does on a flat chain).
	var winners []Attempt
	var winnerChild []int
	for i, ord := range childOrders {
		if len(ord) == 0 {
			continue
		}
		winners = append(winners, ord[0])
		winnerChild = append(winnerChild, i)
	}
	if len(winners) > 1 {
		winners = e.rankAttemptsWithContext(strategy, winners, hookCtx)
	}
	// Map the ranked winners back to their children. Attempt
	// is two strings, so == is exact; a duplicate winner
	// (the same provider+model heading two children) maps
	// to the first child still unused with that head.
	used := make(map[int]bool, len(winnerChild))
	rankedChildren := make([]int, 0, len(winnerChild))
	for _, w := range winners {
		for _, ci := range winnerChild {
			if used[ci] {
				continue
			}
			if childOrders[ci][0] == w {
				used[ci] = true
				rankedChildren = append(rankedChildren, ci)
				break
			}
		}
	}
	var out []Attempt
	for _, ci := range rankedChildren {
		out = append(out, childOrders[ci]...)
	}
	// Children with no winner (no matching attempt in the
	// chain) contribute nothing; children whose winner was
	// somehow unranked (cannot happen — every winner is
	// ranked) would be dropped, so guard explicitly.
	for i, ord := range childOrders {
		if len(ord) == 0 || used[i] {
			continue
		}
		out = append(out, ord...)
	}
	// Dedup: an attempt named by two leaves is tried once.
	seen := make(map[Attempt]bool, len(out))
	deduped := out[:0]
	for _, att := range out {
		if seen[att] {
			continue
		}
		seen[att] = true
		deduped = append(deduped, att)
	}
	return deduped, nil
}

// rankAttempts reorders candidates under one strategy name
// using the engine's strategy hook (falling back to OrderHook,
// then to the input order when neither is set) — the same
// ranking the flat path applies. The per-request variant is
// rankAttemptsWithContext; this wrapper runs it with a zero
// context, so a per-request hook's cacheability gate stays
// closed here.
func (e *Engine) rankAttempts(strategy string, candidates []Attempt) []Attempt {
	// Context-free entry point: the per-request hook (when
	// installed) runs with a zero context — see
	// orderAttemptsByTree.
	return e.rankAttemptsWithContext(strategy, candidates, StrategyHookContext{})
}

// rankAttemptsWithContext reorders candidates under one strategy
// name for one request, using the engine's per-request strategy
// hook when set (falling back to the context-free StrategyHook,
// then OrderHook, then the input order) — the same ranking the
// flat path applies. Two exceptions, both intercepted before the
// hook exactly as the flat path does in orderAttemptsWithContext:
// "quota-share" is ordered with the engine's own DRR (see
// orderQuotaShare, cascade.go), and "cascade" is ordered with
// applyCascadeOrder over the live auto-catalog tiers (see
// cascadeTierFn, cascade.go) — the hook factory has no case for
// either, so without these a tree-ordered node naming one of them
// would rank its winners by input order.
func (e *Engine) rankAttemptsWithContext(strategy string, candidates []Attempt, hookCtx StrategyHookContext) []Attempt {
	if strategy == quotaShareStrategy {
		return e.orderQuotaShare(candidates)
	}
	if strategy == cascadeStrategy {
		return applyCascadeOrder(candidates, e.cascadeTierFn())
	}
	if e.cfg.StrategyHookForRequest != nil {
		if hook := e.cfg.StrategyHookForRequest(strategy, hookCtx); hook != nil {
			return hook(candidates)
		}
	}
	if e.cfg.StrategyHook != nil {
		if hook := e.cfg.StrategyHook(strategy); hook != nil {
			return hook(candidates)
		}
	}
	if e.cfg.OrderHook != nil {
		return e.cfg.OrderHook(candidates)
	}
	return candidates
}

// noteTreeServed records, on the route trace, the tree path to
// the leaf that served this request — e.g. ["cheapest",
// "fastest", "prov-a"]. rootStrategy is the strategy that
// actually ordered the chain at the root: the tree's own root
// strategy, or the per-request override (contract sort /
// auto:<axis> alias) when one replaced it. A nil tree or a
// serving provider no leaf names records nothing: an absent
// path is more honest than an invented one. The dashboard
// renders it from the trace feed and from the strategies
// feed's last-tree-path.
func noteTreeServed(trace *routetrace.RouteTrace, tree *strategytree.Node, provider, model, rootStrategy string) {
	if trace == nil || tree == nil {
		return
	}
	if path := strategytree.LeafPathWithRoot(tree, provider, model, rootStrategy); len(path) > 0 {
		trace.StrategyTreePath = path
	}
}

// treeRootStrategy returns the strategy that actually orders a
// tree-ordered chain at its root: the per-request override when
// one replaced it, else the tree's own root strategy ("priority"
// when the root names none). It also reports whether a tree
// orders this request at all — false when no tree is configured
// for the model, or when an explicit contract order skips the
// tree entirely. Callers use it to name the strategy the trace
// and the last-routing record carry, so both name the strategy
// that ordered the chain, never the flat strategy the tree
// replaced.
func (e *Engine) treeRootStrategy(model, reqStrategy string, contractHasOrder bool) (tree *strategytree.Node, root string, ok bool) {
	tree = e.strategyTreeFor(model)
	if tree == nil || contractHasOrder {
		return nil, "", false
	}
	if reqStrategy != "" {
		return tree, strategytree.CanonicalStrategy(reqStrategy), true
	}
	if tree.Strategy != "" {
		return tree, strategytree.CanonicalStrategy(tree.Strategy), true
	}
	return tree, "priority", true
}
