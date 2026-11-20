package strategy

import (
	"math/rand"
	"sync"
	"testing"
)

// TestChainStateGetOrCreateSameInstance: the same chain key returns
// the same *ChainState; a different key returns a different one.
func TestChainStateGetOrCreateSameInstance(t *testing.T) {
	s := NewChainStateStore()
	a1 := s.GetOrCreate("chain-a")
	a2 := s.GetOrCreate("chain-a")
	if a1 != a2 {
		t.Fatal("GetOrCreate(chain-a) returned different instances")
	}
	b := s.GetOrCreate("chain-b")
	if b == a1 {
		t.Fatal("GetOrCreate(chain-b) returned chain-a's state")
	}
	if a1.RR == nil || a1.Deck == nil {
		t.Fatal("ChainState has nil RR or Deck")
	}
}

// TestChainStateRoundRobinIsolation: two chains rotate independently.
// Chain A advances through its cycle while chain B still starts at
// its own first target.
func TestChainStateRoundRobinIsolation(t *testing.T) {
	s := NewChainStateStore()
	targetsA := []OrderingTarget{
		{Provider: "a1", Model: "m1"},
		{Provider: "a2", Model: "m2"},
		{Provider: "a3", Model: "m3"},
	}
	targetsB := []OrderingTarget{
		{Provider: "b1", Model: "m1"},
		{Provider: "b2", Model: "m2"},
		{Provider: "b3", Model: "m3"},
	}
	a := s.GetOrCreate("chain-a")
	b := s.GetOrCreate("chain-b")

	if got := a.RR.Order(targetsA)[0].Provider; got != "a1" {
		t.Fatalf("chain-a 1st leader = %s, want a1", got)
	}
	if got := a.RR.Order(targetsA)[0].Provider; got != "a2" {
		t.Fatalf("chain-a 2nd leader = %s, want a2", got)
	}
	// Chain B is untouched by chain A's rotation: it starts at b1.
	if got := b.RR.Order(targetsB)[0].Provider; got != "b1" {
		t.Fatalf("chain-b 1st leader = %s, want b1", got)
	}
	// Chain A continues its own cycle (a3), chain B continues its own (b2).
	if got := a.RR.Order(targetsA)[0].Provider; got != "a3" {
		t.Fatalf("chain-a 3rd leader = %s, want a3", got)
	}
	if got := b.RR.Order(targetsB)[0].Provider; got != "b2" {
		t.Fatalf("chain-b 2nd leader = %s, want b2", got)
	}
}

// TestChainStateDeckIsolation: two chains deal from independent
// decks. Interleaved calls still give each chain a full cycle with
// every target leading exactly once — no repeat until exhausted, and
// no cross-chain interference.
func TestChainStateDeckIsolation(t *testing.T) {
	s := NewChainStateStore()
	targets := []OrderingTarget{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
		{Provider: "p3", Model: "m3"},
	}
	a := s.GetOrCreate("chain-a")
	b := s.GetOrCreate("chain-b")
	rndA := rand.New(rand.NewSource(42))
	rndB := rand.New(rand.NewSource(7))

	// Interleave the two decks call by call.
	seenA := make(map[string]int)
	seenB := make(map[string]int)
	for i := 0; i < len(targets); i++ {
		orderedA := a.Deck.Order(targets, rndA)
		orderedB := b.Deck.Order(targets, rndB)
		if len(orderedA) != len(targets) || len(orderedB) != len(targets) {
			t.Fatalf("Deck.Order returned %d/%d targets, want %d", len(orderedA), len(orderedB), len(targets))
		}
		seenA[orderedA[0].Provider]++
		seenB[orderedB[0].Provider]++
	}
	for _, tc := range []struct {
		name string
		seen map[string]int
	}{
		{"chain-a", seenA},
		{"chain-b", seenB},
	} {
		for _, p := range []string{"p1", "p2", "p3"} {
			if tc.seen[p] != 1 {
				t.Fatalf("%s: provider %s led %d times in one cycle, want exactly 1 (seen=%v)", tc.name, p, tc.seen[p], tc.seen)
			}
		}
	}
}

// TestChainStateConcurrentGetOrCreate: many goroutines asking for the
// same chain key all get the same instance; -race must stay clean.
func TestChainStateConcurrentGetOrCreate(t *testing.T) {
	s := NewChainStateStore()
	const goroutines = 32
	got := make([]*ChainState, goroutines)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			got[g] = s.GetOrCreate("chain-a")
		}(g)
	}
	wg.Wait()
	for g := 1; g < goroutines; g++ {
		if got[g] != got[0] {
			t.Fatalf("goroutine %d got a different ChainState instance", g)
		}
	}
}

// TestChainStateConcurrentRoundRobin: round-robin rotation from many
// goroutines stays race-clean and every call still returns a full
// permutation of the targets.
func TestChainStateConcurrentRoundRobin(t *testing.T) {
	s := NewChainStateStore()
	targets := []OrderingTarget{
		{Provider: "p1", Model: "m1"},
		{Provider: "p2", Model: "m2"},
		{Provider: "p3", Model: "m3"},
	}
	st := s.GetOrCreate("chain-a")
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				ordered := st.RR.Order(targets)
				if len(ordered) != len(targets) {
					t.Errorf("RR.Order returned %d targets, want %d", len(ordered), len(targets))
					return
				}
			}
		}()
	}
	wg.Wait()
}
