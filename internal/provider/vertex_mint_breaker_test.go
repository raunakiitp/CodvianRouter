package provider

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// M4.7b — token-refresh breaker: after 5 consecutive mint failures,
// minting cools down for 30 minutes (shrunk here) instead of
// hammering the token endpoint once per request.

// TestVertexMintBreakerTripsAfterFiveFailures: five rejected
// exchanges trip the breaker — the sixth mint attempt never
// reaches the token endpoint (hits stays 5) and fails fast with
// the cooldown error.
func TestVertexMintBreakerTripsAfterFiveFailures(t *testing.T) {
	oldCooldown := vertexMintBreakerCooldown
	vertexMintBreakerCooldown = 30 * time.Minute
	t.Cleanup(func() { vertexMintBreakerCooldown = oldCooldown })
	_, _, tok, vc := vertexTestServers(t)
	tok.errStatus = http.StatusInternalServerError
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, err := vc.accessToken(ctx)
		if err == nil {
			t.Fatalf("mint %d: want the exchange error, got nil", i+1)
		}
		if IsVertexMintCoolingDown(err) {
			t.Fatalf("mint %d: cooling down already (%v), want the real exchange error (trip is at 5)", i+1, err)
		}
	}
	if got := tok.hits.Load(); got != 5 {
		t.Fatalf("token endpoint hits = %d, want 5 (one exchange per mint attempt)", got)
	}
	_, err := vc.accessToken(ctx)
	if !IsVertexMintCoolingDown(err) {
		t.Fatalf("6th mint: err = %v, want the cooling-down fast failure", err)
	}
	if got := tok.hits.Load(); got != 5 {
		t.Fatalf("token endpoint hits = %d after the 6th mint, want still 5 (no exchange attempted during cooldown)", got)
	}
	// The entry itself is cooling down (the global count is
	// shared with other tests' entries, so it is asserted as a
	// delta, not an absolute).
	sa, _, err := parseVertexServiceAccount(vc.apiKey)
	if err != nil {
		t.Fatalf("parse service account: %v", err)
	}
	entry := vertexTokenCacheEntryFor(vertexKeyFingerprint(sa))
	entry.mu.Lock()
	cooling := time.Now().Before(entry.mintCooldownUntil)
	entry.mu.Unlock()
	if !cooling {
		t.Fatalf("entry not cooling down after 5 failures, want mintCooldownUntil in the future")
	}
	if got := VertexMintCoolingDown(); got < 1 {
		t.Fatalf("VertexMintCoolingDown() = %d, want >= 1 (this key is cooling down)", got)
	}
}

// TestVertexMintBreakerCooldownExpires: once the cooldown passes,
// minting resumes — and a healthy endpoint mints fine again.
func TestVertexMintBreakerCooldownExpires(t *testing.T) {
	oldCooldown := vertexMintBreakerCooldown
	vertexMintBreakerCooldown = 50 * time.Millisecond
	t.Cleanup(func() { vertexMintBreakerCooldown = oldCooldown })
	_, _, tok, vc := vertexTestServers(t)
	tok.errStatus = http.StatusInternalServerError
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := vc.accessToken(ctx); err == nil {
			t.Fatalf("mint %d: want the exchange error, got nil", i+1)
		}
	}
	if _, err := vc.accessToken(ctx); !IsVertexMintCoolingDown(err) {
		t.Fatalf("mint during cooldown: err = %v, want cooling down", err)
	}
	time.Sleep(60 * time.Millisecond)
	tok.errStatus = 0
	got, err := vc.accessToken(ctx)
	if err != nil {
		t.Fatalf("mint after cooldown: %v, want success", err)
	}
	if got != "tok-vertex-abc" {
		t.Fatalf("mint after cooldown = %q, want the mock token", got)
	}
	if hits := tok.hits.Load(); hits != 6 {
		t.Fatalf("token endpoint hits = %d, want 6 (5 failed + 1 resumed)", hits)
	}
	// The entry's cooldown is cleared by the successful mint
	// (the global count is shared with other tests' entries, so
	// the entry itself is the assertion).
	sa, _, err := parseVertexServiceAccount(vc.apiKey)
	if err != nil {
		t.Fatalf("parse service account: %v", err)
	}
	entry := vertexTokenCacheEntryFor(vertexKeyFingerprint(sa))
	entry.mu.Lock()
	cooling := time.Now().Before(entry.mintCooldownUntil)
	entry.mu.Unlock()
	if cooling {
		t.Fatalf("entry still cooling down after a successful mint, want the cooldown cleared")
	}
}

// TestVertexMintBreakerSuccessResets: a success clears the failure
// count — four failures, one success, then four more failures do
// NOT trip the breaker; only the fifth consecutive failure after
// the success does.
func TestVertexMintBreakerSuccessResets(t *testing.T) {
	oldCooldown := vertexMintBreakerCooldown
	vertexMintBreakerCooldown = 30 * time.Minute
	t.Cleanup(func() { vertexMintBreakerCooldown = oldCooldown })
	_, _, tok, vc := vertexTestServers(t)
	ctx := context.Background()
	sa, _, err := parseVertexServiceAccount(vc.apiKey)
	if err != nil {
		t.Fatalf("parse service account: %v", err)
	}
	entry := vertexTokenCacheEntryFor(vertexKeyFingerprint(sa))
	clearToken := func() {
		entry.mu.Lock()
		entry.token = ""
		entry.mu.Unlock()
	}
	tok.errStatus = http.StatusInternalServerError
	for i := 0; i < 4; i++ {
		if _, err := vc.accessToken(ctx); err == nil {
			t.Fatalf("mint %d: want the exchange error, got nil", i+1)
		}
	}
	tok.errStatus = 0
	if _, err := vc.accessToken(ctx); err != nil {
		t.Fatalf("recovery mint: %v, want success", err)
	}
	clearToken() // force a re-mint so the count is exercised again
	tok.errStatus = http.StatusInternalServerError
	for i := 0; i < 4; i++ {
		_, err := vc.accessToken(ctx)
		if err == nil {
			t.Fatalf("post-recovery mint %d: want the exchange error, got nil", i+1)
		}
		if IsVertexMintCoolingDown(err) {
			t.Fatalf("post-recovery mint %d: cooling down (%v), want the real error (only 4 since the success)", i+1, err)
		}
	}
	// The 5th consecutive failure after the success trips the
	// breaker — but the trip itself still returns the exchange
	// error (the cooldown gates the NEXT mint, not the one that
	// tripped it); the call after that fails fast.
	if _, err := vc.accessToken(ctx); err == nil || IsVertexMintCoolingDown(err) {
		t.Fatalf("5th post-recovery mint: err = %v, want the exchange error (the trip returns the failure that caused it)", err)
	}
	if _, err := vc.accessToken(ctx); !IsVertexMintCoolingDown(err) {
		t.Fatalf("6th post-recovery mint: err = %v, want cooling down (5 consecutive failures)", err)
	}
}
