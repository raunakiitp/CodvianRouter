package coalescing

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestRequestKeySensitiveToRoutingContract pins the CHECKLIST 157
// rule: two requests that differ only in their routing contract
// must never share one single-flight. A waiter would otherwise be
// served by a provider its own contract forbade.
func TestRequestKeySensitiveToRoutingContract(t *testing.T) {
	base, err := RequestKey(testReq("hello"))
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	onlyA := testReq("hello")
	onlyA.Routing = &provider.RequestRouting{Only: []string{"a"}}
	ka, err := RequestKey(onlyA)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	if ka == base {
		t.Fatalf("a contracted request hashed the same as an uncontracted one — it would share a flight")
	}
	onlyB := testReq("hello")
	onlyB.Routing = &provider.RequestRouting{Only: []string{"b"}}
	kb, err := RequestKey(onlyB)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	if kb == ka {
		t.Fatalf("different only-sets hashed the same — they would share a flight")
	}
	// The same contract hashes the same: identical contracted
	// requests still coalesce with each other.
	again := testReq("hello")
	again.Routing = &provider.RequestRouting{Only: []string{"a"}}
	ka2, err := RequestKey(again)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	if ka2 != ka {
		t.Fatalf("identical contracts hashed differently %q vs %q", ka, ka2)
	}
	// Order matters: a different try order is a different request.
	ordered := testReq("hello")
	ordered.Routing = &provider.RequestRouting{Order: []string{"b", "a"}}
	ko, err := RequestKey(ordered)
	if err != nil {
		t.Fatalf("RequestKey: %v", err)
	}
	if ko == ka {
		t.Fatalf("an order contract hashed the same as an only contract")
	}
}
