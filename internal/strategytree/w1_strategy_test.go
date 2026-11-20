// W1.3 (M1.11): the default global strategy is "priority" (declared
// chain order, no re-ranking), and "priority" + "code" are valid
// strategy-tree names — the allowlist must accept them.
package strategytree

import (
	"strings"
	"testing"
)

func TestW1PriorityAndCodeAreValidStrategies(t *testing.T) {
	for _, s := range []string{"priority", "code", " Priority ", "CODE"} {
		if !IsValidStrategy(s) {
			t.Errorf("IsValidStrategy(%q) = false, want true (M1.11)", s)
		}
	}
	if got := CanonicalStrategy("priority"); got != "priority" {
		t.Errorf("CanonicalStrategy(priority) = %q, want priority", got)
	}
	if got := CanonicalStrategy("code"); got != "code" {
		t.Errorf("CanonicalStrategy(code) = %q, want code", got)
	}
	for _, s := range []string{"priority", "code"} {
		tree := &Node{Strategy: s, Children: []*Node{{Provider: "a"}}}
		if err := Validate(tree); err != nil {
			t.Errorf("Validate(%q root) = %v, want nil", s, err)
		}
	}
	// An empty root strategy reads as the default, "priority"
	// (M1.11 changed it from "balanced").
	empty := &Node{Children: []*Node{{Provider: "x"}}}
	if got := LeafPath(empty, "x", "m"); strings.Join(got, " > ") != "priority > x" {
		t.Errorf("LeafPath(empty-root) = %v, want [priority x]", got)
	}
}
