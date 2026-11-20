package degrade

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestPrefixFreezeFreezeChecker proves the adapter the Apply
// path consumes (parity tracker Area 10 row :939): FreezeChecker
// turns the observer into the predicate
// compress.ApplyOptions.FrozenPrefix takes, so the router/server
// pass-through is one line (pass freeze.FreezeChecker()) with no
// new types. A disabled observer — and a nil receiver — always
// report false, never panic.
func TestPrefixFreezeFreezeChecker(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: "You are a helpful assistant. Answer clearly."},
		{Role: "user", Content: "Hello."},
	}
	freeze := NewPrefixFreeze(true, 0)
	check := freeze.FreezeChecker()
	if check == nil {
		t.Fatalf("FreezeChecker must return a predicate, got nil")
	}
	if got := check(msgs); got {
		t.Fatalf("fresh observer must not freeze anything")
	}
	freeze.Observe(msgs)
	freeze.Observe(msgs)
	if got := check(msgs); got {
		t.Fatalf("below-threshold prefix must not freeze")
	}
	freeze.Observe(msgs)
	if got := check(msgs); !got {
		t.Fatalf("prefix observed 3 times (default threshold) must freeze")
	}
	other := []provider.Message{
		{Role: "system", Content: "A different system prompt entirely."},
		{Role: "user", Content: "Hello."},
	}
	if got := check(other); got {
		t.Fatalf("unseen prefix must not freeze")
	}
	// Disabled observer never freezes, even after observations
	// (Observe is a no-op when disabled).
	off := NewPrefixFreeze(false, 0)
	off.Observe(msgs)
	off.Observe(msgs)
	off.Observe(msgs)
	if got := off.FreezeChecker()(msgs); got {
		t.Fatalf("disabled observer must never freeze")
	}
	// Nil receiver is a safe always-false predicate, never a panic.
	var nilFreeze *PrefixFreeze
	if got := nilFreeze.FreezeChecker()(msgs); got {
		t.Fatalf("nil observer must never freeze")
	}
}
