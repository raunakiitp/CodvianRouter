// Package strategytree is the nestable strategy-tree config model
// for Codvian Router (CHECKLIST 177): a strategy node can contain
// other strategy nodes, e.g. cheapest-of[fastest-of[A, B], C].
//
// A tree is a JSON value in the config file's routing block
// (strategy_tree for every chain, strategy_trees[model] for one
// chain). A node is either:
//
//   - an internal node: a strategy name plus children — it picks
//     among its children by that strategy, and
//   - a leaf: a provider (plus an optional model) — it names one
//     concrete serving target.
//
// The tree is evaluated recursively by internal/router: each node
// orders its children's winners by its own strategy, and the full
// ordered list of each child follows its winner, so a nested node
// contributes its whole subtree in the order it ranked. Leaves
// that match no attempt in the chain contribute nothing; attempts
// no leaf names are appended after the tree-ordered ones, in
// chain order — a tree never silently drops a fallback target.
//
// This package is a leaf: stdlib only, no internal imports, so
// internal/config can own the config shape and internal/router
// can evaluate it without an import cycle. Strategy names are
// validated here against the same eight canonical names the
// strategy package owns (priority, balanced, cheapest, fastest,
// most-reliable, least-latency, weighted, custom-weights, code,
// round-robin, random, strict-random, fill-first, least-used,
// p2c, quota-weighted, headroom, expiry-first, reset-aware,
// reset-window, lkgp, context-optimized, cache-optimized,
// context-relay, cost-optimized, ship-fast, cost-saver,
// quality-first, offline-friendly, reliability-first, chaos-mode)
// plus its aliases —
// the list is duplicated deliberately and pinned by test, for
// the same reason the router duplicates the auto:<axis> mapping.
//
// Depth is capped at MaxDepth (8 nodes root-to-leaf). A cycle —
// only constructible in memory, never from JSON, where every
// node is a fresh pointer — and any depth overrun are honest
// config errors naming the node path (root.children[0]...),
// never a stack overflow and never a silent flatten.
package strategytree

import (
	"fmt"
	"strings"
)

// MaxDepth is the deepest a strategy tree may nest, counted in
// nodes from the root (root = depth 1). Eight is far deeper than
// any useful routing tree (three levels already express
// cheapest-of[fastest-of[...], ...]) and small enough that
// evaluation stays trivially cheap.
const MaxDepth = 8

// Node is one node of a strategy tree.
//
// Strategy names the strategy this node picks its children by
// (empty = priority). It is only meaningful on an internal node;
// a leaf with a strategy is a config error.
//
// Provider names the serving provider of a leaf (empty on an
// internal node). Model optionally narrows the leaf to one model
// of that provider; empty means every model the provider serves
// in this chain.
//
// Children holds the child nodes of an internal node; empty on a
// leaf. A node with both children and a provider is a config
// error — it cannot be both a picker and a target.
type Node struct {
	Strategy string  `json:"strategy,omitempty"`
	Provider string  `json:"provider,omitempty"`
	Model    string  `json:"model,omitempty"`
	Children []*Node `json:"children,omitempty"`
}

// IsLeaf reports whether n is a leaf (no children).
func (n *Node) IsLeaf() bool {
	return n == nil || len(n.Children) == 0
}

// validStrategies is the allowlist of strategy names a tree node
// may carry: the eight canonical names plus the aliases the
// strategy package accepts. It mirrors strategy.Parse without
// importing it (that would be a cycle: strategy imports router,
// router imports config, config imports this package).
var validStrategies = map[string]bool{
	"priority": true, "code": true,
	"balanced": true, "cheapest": true, "cheap": true,
	"fastest": true, "fast": true,
	"most-reliable": true, "reliable": true,
	"least-latency": true, "latency": true,
	"weighted": true, "custom-weights": true, "custom": true,
	"round-robin": true, "random": true, "strict-random": true,
	"fill-first": true, "least-used": true, "p2c": true,
	"quota-weighted": true, "quota-share": true, "headroom": true, "expiry-first": true,
	"reset-aware": true, "reset-window": true,
	"lkgp": true, "context-optimized": true, "cache-optimized": true,
	"context-relay":  true,
	"cost-optimized": true, "eco": true,
	"ship-fast": true, "cost-saver": true, "quality-first": true,
	"offline-friendly": true, "reliability-first": true, "chaos-mode": true, "fusion": true,
}

// IsValidStrategy reports whether s names a known strategy
// (canonical name or alias, case-insensitive). Empty is not
// valid here — callers treat empty as "priority" before asking.
func IsValidStrategy(s string) bool {
	return validStrategies[strings.ToLower(strings.TrimSpace(s))]
}

// Validate checks a whole tree: every node's strategy is known
// (or empty), every leaf names a provider, no node is both a
// picker and a target, no node is shared or cyclic, and the
// depth never exceeds MaxDepth. Every error names the node path
// (root, root.children[0], ...) so the operator can find the
// bad node in their config file. A nil root is an error —
// callers that mean "no tree" pass no tree at all, never nil.
func Validate(root *Node) error {
	if root == nil {
		return fmt.Errorf("strategy tree is empty")
	}
	seen := make(map[*Node]string)
	return validateNode(root, "root", 1, seen)
}

func validateNode(n *Node, path string, depth int, seen map[*Node]string) error {
	if n == nil {
		return fmt.Errorf("strategy tree node %s is null", path)
	}
	if prev, ok := seen[n]; ok {
		return fmt.Errorf("strategy tree node %s reuses the node at %s (a tree node may appear only once — a cycle or shared subtree is not allowed)", path, prev)
	}
	seen[n] = path
	if depth > MaxDepth {
		return fmt.Errorf("strategy tree node %s exceeds the maximum depth of %d (nesting this deep is a config error, not a deeper tree)", path, MaxDepth)
	}
	if n.Strategy != "" && !IsValidStrategy(n.Strategy) {
		return fmt.Errorf("strategy tree node %s: strategy %q is not a known strategy (priority, balanced, cheapest, fastest, most-reliable, least-latency, weighted, custom-weights, code, round-robin, random, strict-random, fill-first, least-used, p2c, quota-weighted, quota-share, headroom, expiry-first, reset-aware, reset-window, lkgp, context-optimized, cache-optimized, context-relay, cost-optimized, ship-fast, cost-saver, quality-first, offline-friendly, reliability-first, chaos-mode, fusion)", path, n.Strategy)
	}
	if len(n.Children) > 0 {
		if n.Provider != "" || n.Model != "" {
			return fmt.Errorf("strategy tree node %s names provider %q and has %d children — a node is either a strategy picker (children) or a leaf target (provider), never both", path, n.Provider, len(n.Children))
		}
		for i, c := range n.Children {
			if err := validateNode(c, fmt.Sprintf("%s.children[%d]", path, i), depth+1, seen); err != nil {
				return err
			}
		}
		return nil
	}
	// Leaf.
	if n.Strategy != "" {
		return fmt.Errorf("strategy tree node %s: leaf for provider %q carries strategy %q — a leaf has no children to pick among, so a strategy on it is a config error", path, n.Provider, n.Strategy)
	}
	if n.Provider == "" {
		return fmt.Errorf("strategy tree node %s is a leaf with no provider — a leaf must name a provider", path)
	}
	return nil
}

// Clone returns a deep copy of a tree (nil clones to nil), so a
// caller can hold and mutate its own copy without affecting the
// config the router started from.
func Clone(root *Node) *Node {
	if root == nil {
		return nil
	}
	out := *root
	if root.Children != nil {
		out.Children = make([]*Node, len(root.Children))
		for i, c := range root.Children {
			out.Children[i] = Clone(c)
		}
	}
	return &out
}

// LeafPath returns the path from the root to the leaf serving
// (provider, model): the strategy of every internal node on the
// way down (empty strategy reads as "priority"), ending with
// the leaf's provider ID — e.g. ["cheapest", "fastest", "prov-a"]
// for cheapest-of[fastest-of[prov-a, ...], ...]. A leaf's Model,
// when set, must equal the requested model; a leaf with no
// model matches any model of its provider. It returns nil when
// no leaf in the tree serves that (provider, model) pair —
// callers render that as "no tree path", never an invented one.
func LeafPath(root *Node, provider, model string) []string {
	return LeafPathWithRoot(root, provider, model, "")
}

// LeafPathWithRoot is LeafPath with the root node's strategy
// replaced by rootOverride (a per-request contract sort or
// auto:<axis> alias that replaced the root strategy for one
// request): the path then names the strategy that actually
// ordered the chain at the root, not the one in the file. An
// empty override means the tree's own root strategy governs.
// The override is canonicalised (cheap -> cheapest), so the
// path never shows an alias where the engine applied the
// canonical strategy.
func LeafPathWithRoot(root *Node, provider, model, rootOverride string) []string {
	path, ok := leafPath(root, provider, model, nil, rootOverride, 1)
	if !ok {
		return nil
	}
	return path
}

// canonicalStrategies maps every accepted strategy spelling to
// its canonical name (see IsValidStrategy).
var canonicalStrategies = map[string]string{
	"priority": "priority", "code": "code",
	"balanced": "balanced", "cheapest": "cheapest", "cheap": "cheapest",
	"fastest": "fastest", "fast": "fastest",
	"most-reliable": "most-reliable", "reliable": "most-reliable",
	"least-latency": "least-latency", "latency": "least-latency",
	"weighted": "weighted",
	"fusion":   "fusion",
}

// CanonicalStrategy returns the canonical name for a strategy
// spelling (aliases mapped, case-insensitive); "" and unknown
// names come back unchanged.
func CanonicalStrategy(s string) string {
	if c, ok := canonicalStrategies[strings.ToLower(strings.TrimSpace(s))]; ok {
		return c
	}
	return s
}

func leafPath(n *Node, provider, model string, acc []string, rootOverride string, depth int) ([]string, bool) {
	if n == nil {
		return nil, false
	}
	// Copy acc before extending: append would otherwise share
	// the backing array between sibling subtrees, and a later
	// sibling's step could overwrite a path already found.
	next := make([]string, len(acc)+1)
	copy(next, acc)
	if len(n.Children) == 0 {
		if n.Provider == provider && (n.Model == "" || n.Model == model) {
			next[len(acc)] = n.Provider
			return next, true
		}
		return nil, false
	}
	strat := n.Strategy
	if depth == 1 && rootOverride != "" {
		strat = CanonicalStrategy(rootOverride)
	}
	if strat == "" {
		strat = "priority"
	}
	next[len(acc)] = strat
	for _, c := range n.Children {
		if p, ok := leafPath(c, provider, model, next, "", depth+1); ok {
			return p, true
		}
	}
	return nil, false
}
