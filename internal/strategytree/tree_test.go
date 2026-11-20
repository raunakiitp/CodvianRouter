package strategytree

import (
	"strings"
	"testing"
)

// tree is cheapest-of[fastest-of[A, B], C]: the example from the
// slice task.
func exampleTree() *Node {
	return &Node{Strategy: "cheapest", Children: []*Node{
		{Strategy: "fastest", Children: []*Node{
			{Provider: "prov-a", Model: "m"},
			{Provider: "prov-b", Model: "m"},
		}},
		{Provider: "prov-c", Model: "m"},
	}}
}

func TestValidateExampleTree(t *testing.T) {
	if err := Validate(exampleTree()); err != nil {
		t.Fatalf("Validate(example) = %v, want nil", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := []struct {
		name string
		tree *Node
		want string // substring of the error, and the node path must appear
	}{
		{"nil root", nil, "empty"},
		{"unknown strategy", &Node{Strategy: "weird", Children: []*Node{{Provider: "a"}}}, "root"},
		{"unknown strategy nested", &Node{Strategy: "cheapest", Children: []*Node{
			{Strategy: "nope", Children: []*Node{{Provider: "a"}}},
		}}, "root.children[0]"},
		{"leaf without provider", &Node{Strategy: "cheapest", Children: []*Node{{}}}, "root.children[0]"},
		{"leaf without provider at root", &Node{}, "root"},
		{"picker and leaf", &Node{Strategy: "cheapest", Provider: "a", Children: []*Node{{Provider: "b"}}}, "root"},
		{"strategy on leaf", &Node{Provider: "a", Strategy: "fastest"}, "root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.tree)
			if err == nil {
				t.Fatalf("Validate = nil, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestValidateDepthCap(t *testing.T) {
	// A chain of MaxDepth internal nodes plus one leaf is
	// exactly at the cap (leaf at depth MaxDepth+1? no: root is
	// depth 1, so MaxDepth-1 internal nodes + leaf = depth
	// MaxDepth — valid; one more internal node tips over).
	atCap := &Node{Strategy: "balanced"}
	cur := atCap
	for i := 0; i < MaxDepth-2; i++ {
		next := &Node{Strategy: "balanced"}
		cur.Children = []*Node{next}
		cur = next
	}
	cur.Children = []*Node{{Provider: "a"}}
	if err := Validate(atCap); err != nil {
		t.Fatalf("Validate(at-cap tree, leaf at depth %d) = %v, want nil", MaxDepth, err)
	}
	over := &Node{Strategy: "balanced"}
	cur = over
	for i := 0; i < MaxDepth-1; i++ {
		next := &Node{Strategy: "balanced"}
		cur.Children = []*Node{next}
		cur = next
	}
	cur.Children = []*Node{{Provider: "a"}}
	err := Validate(over)
	if err == nil {
		t.Fatalf("Validate(over-cap tree) = nil, want a depth error")
	}
	if !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("Validate(over-cap) error = %q, want it to name the maximum depth", err)
	}
}

func TestValidateCycleAndSharedNode(t *testing.T) {
	// A node that contains itself: JSON can never express this,
	// but an in-memory tree can — validation must catch it
	// instead of recursing forever.
	cyclic := &Node{Strategy: "balanced"}
	cyclic.Children = []*Node{cyclic}
	if err := Validate(cyclic); err == nil {
		t.Fatalf("Validate(cyclic) = nil, want a cycle error")
	} else if !strings.Contains(err.Error(), "reuses") {
		t.Fatalf("Validate(cyclic) error = %q, want it to name the reuse", err)
	}

	// The same child pointer under two parents is the same
	// defect in miniature: one node may appear only once.
	shared := &Node{Provider: "a"}
	dag := &Node{Strategy: "balanced", Children: []*Node{
		{Strategy: "fastest", Children: []*Node{shared}},
		{Strategy: "cheapest", Children: []*Node{shared}},
	}}
	if err := Validate(dag); err == nil {
		t.Fatalf("Validate(shared-child) = nil, want a reuse error")
	}
}

func TestCloneDeep(t *testing.T) {
	orig := exampleTree()
	cp := Clone(orig)
	if cp == orig || cp.Children[0] == orig.Children[0] {
		t.Fatalf("Clone shares pointers with the original")
	}
	cp.Children[0].Children[0].Provider = "prov-changed"
	if orig.Children[0].Children[0].Provider != "prov-a" {
		t.Fatalf("mutating the clone changed the original")
	}
	if Clone(nil) != nil {
		t.Fatalf("Clone(nil) != nil")
	}
}

func TestLeafPath(t *testing.T) {
	tree := exampleTree()
	if got := LeafPath(tree, "prov-b", "m"); strings.Join(got, " > ") != "cheapest > fastest > prov-b" {
		t.Fatalf("LeafPath(prov-b) = %v, want [cheapest fastest prov-b]", got)
	}
	if got := LeafPath(tree, "prov-c", "m"); strings.Join(got, " > ") != "cheapest > prov-c" {
		t.Fatalf("LeafPath(prov-c) = %v, want [cheapest prov-c]", got)
	}
	// A leaf with no model matches any model of its provider.
	noModel := &Node{Strategy: "balanced", Children: []*Node{{Provider: "x"}}}
	if got := LeafPath(noModel, "x", "anything"); len(got) != 2 {
		t.Fatalf("LeafPath(model-less leaf) = %v, want [balanced x]", got)
	}
	// A leaf pinned to another model does not match.
	if got := LeafPath(tree, "prov-a", "other-model"); got != nil {
		t.Fatalf("LeafPath(prov-a, other-model) = %v, want nil", got)
	}
	if got := LeafPath(tree, "prov-missing", "m"); got != nil {
		t.Fatalf("LeafPath(missing provider) = %v, want nil", got)
	}
	if got := LeafPath(nil, "a", "m"); got != nil {
		t.Fatalf("LeafPath(nil) = %v, want nil", got)
	}
}

func TestIsValidStrategy(t *testing.T) {
	for _, s := range []string{"priority", "code", "balanced", "cheapest", "cheap", "fastest", "fast", "most-reliable", "reliable", "least-latency", "latency", "weighted", " Cheapest "} {
		if !IsValidStrategy(s) {
			t.Fatalf("IsValidStrategy(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "weird", "auto"} {
		if IsValidStrategy(s) {
			t.Fatalf("IsValidStrategy(%q) = true, want false", s)
		}
	}
}
