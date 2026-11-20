package cache

// M6.12 decision tests: per-provider isolation is a recorded
// DIVERGENCE, not an unimplemented gap — see the DECISION (M6.12)
// comment on KeyInput in key.go. These tests pin the sharing
// behaviour so a later change to provider-scope the key cannot
// slip in silently: it must update the decision record first.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestM612SameModelSharedAcrossProviders pins the M6.12 sharing
// rule: two requests that differ ONLY in which provider would
// serve them — same tenant, model, messages and generation params
// — derive the SAME cache key, because ScopedKey cannot see the
// provider (provider.ChatRequest has no Provider field, and the
// lookup runs before routing picks one). The two KeyInputs below
// are identical by construction: that identity IS the point —
// there is no provider dimension to vary, so the test pins key
// determinism plus cross-provider sharing through the cache
// itself. The tripwire test below is the guard that stops a
// provider dimension being added silently.
func TestM612SameModelSharedAcrossProviders(t *testing.T) {
	// Request A would route to provider A, request B to provider B
	// (e.g. gpt-4o via OpenAI vs via OpenRouter); the key input
	// the cache ever sees is the same for both.
	a := m1BaseInput()
	b := m1BaseInput()
	ka, err := ScopedKey(a)
	if err != nil {
		t.Fatalf("ScopedKey a: %v", err)
	}
	kb, err := ScopedKey(b)
	if err != nil {
		t.Fatalf("ScopedKey b: %v", err)
	}
	if ka != kb {
		t.Fatalf("same (tenant, model, messages) produced different keys %q vs %q — provider must not be a key dimension per M6.12", ka, kb)
	}
	// And through the cache itself: an answer stored for the
	// request that routed to provider A is served to the identical
	// request that would route to provider B. That cross-provider
	// sharing is the deliberate M6.12 divergence.
	c := NewExactCache(10)
	c.Set(context.Background(), ka, CachedResponse{Content: "answer-a", Model: "gpt-4o"}, time.Hour)
	got, found := c.Get(context.Background(), kb)
	if !found || got.Content != "answer-a" {
		t.Fatalf("cross-provider share broken: found=%v got=%q, want shared entry %q", found, got.Content, "answer-a")
	}
}

// TestM612KeyInputHasNoProviderField is the tripwire for the M6.12
// decision: KeyInput must not gain any provider dimension. Adding
// one would be unfillable at both call sites (degrade.scopedKey and
// relay.scopedMessagesKey run before routing picks a provider),
// so a provider field here would either sit empty (dead code
// that looks like isolation) or be filled with a guess (wrong
// keys on failover). Anyone who wants provider isolation must
// first restructure lookup to run after provider selection (or
// record the serving provider at store time) and update the
// DECISION (M6.12) comment on KeyInput — and this test.
func TestM612KeyInputHasNoProviderField(t *testing.T) {
	typ := reflect.TypeOf(KeyInput{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if strings.Contains(name, "Provider") {
			t.Fatalf("KeyInput gained a provider field %q — see DECISION (M6.12) on KeyInput: provider isolation needs lookup-after-routing, not a field here", name)
		}
	}
}
