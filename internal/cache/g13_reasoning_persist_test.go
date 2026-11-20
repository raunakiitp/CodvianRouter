// G13 — reasoning-cache DB tier + stored-bytes budget tests
// (PARITY.md Area 6 row "Reasoning cache (2 h)").
package cache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// g13ReasoningState builds a state with short visible text but a
// long opaque payload: an Anthropic thinking block whose signature
// dwarfs its text, plus a redacted block.
func g13ReasoningState(t *testing.T) reasoning.State {
	t.Helper()
	return reasoning.State{
		Blocks: []reasoning.Block{
			{Kind: reasoning.KindThinking, Text: "short thought", Signature: strings.Repeat("s", 1000)},
			// Data is JSON (a redacted-payload string) — the
			// persist tier marshals the state, so an invalid
			// blob would fail the write-through.
			{Kind: reasoning.KindRedactedThinking, Data: []byte(`"` + strings.Repeat("x", 500) + `"`)},
		},
		SourceWire:  reasoning.WireAnthropic,
		SourceModel: "claude-sonnet-4-5",
	}
}

func TestG13ReasoningPersistsAcrossReload(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reasoning.db")
	want := g13ReasoningState(t)

	c, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentReasoningCache: %v", err)
	}
	key := ToolCallKey("tenant-1", "call_42")
	c.SetStateForTenant(ctx, key, "tenant-1", "anthropic", "claude-sonnet-4-5", want)
	if c.Size() != 1 {
		t.Fatalf("Size = %d, want 1", c.Size())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen: the entry (signature + redacted payload verbatim)
	// must survive the store -> reload cycle.
	c2, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	got, ok := c2.GetState(ctx, key)
	if !ok {
		t.Fatalf("GetState after reload: miss, want hit")
	}
	if len(got.Blocks) != 2 {
		t.Fatalf("reloaded blocks = %d, want 2", len(got.Blocks))
	}
	if got.Blocks[0].Signature != want.Blocks[0].Signature {
		t.Errorf("reloaded signature differs: len %d, want %d", len(got.Blocks[0].Signature), len(want.Blocks[0].Signature))
	}
	if string(got.Blocks[1].Data) != string(want.Blocks[1].Data) {
		t.Errorf("reloaded redacted Data differs")
	}
	if got.ReasoningText() != want.ReasoningText() {
		t.Errorf("reloaded text = %q, want %q", got.ReasoningText(), want.ReasoningText())
	}
	// Cross-tenant isolation survives the reload too: the same
	// tool-call id under another tenant must miss.
	if _, ok := c2.GetState(ctx, ToolCallKey("tenant-2", "call_42")); ok {
		t.Errorf("cross-tenant GetState after reload: hit, want miss")
	}
}

func TestG13ReasoningExpiredPrunedOnLoad(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reasoning.db")
	c, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{TTL: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("OpenPersistentReasoningCache: %v", err)
	}
	key := ToolCallKey("tenant-1", "call_9")
	c.SetStateForTenant(ctx, key, "tenant-1", "", "m", reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "ephemeral thought"}},
	})
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	c2, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if _, ok := c2.GetState(ctx, key); ok {
		t.Errorf("expired entry reloaded, want pruned on load")
	}
	if c2.Size() != 0 {
		t.Errorf("Size after reload = %d, want 0", c2.Size())
	}
}

func TestG13ReasoningBudgetCountsSignaturesAndRedacted(t *testing.T) {
	ctx := context.Background()
	// A state with 13 chars of visible text but 1500 stored bytes
	// (1000-byte signature + 500-byte redacted payload): under the
	// old text-only accounting it counted as 13 bytes.
	st := g13ReasoningState(t)
	if got := st.StoredBytes(); got < 1500 {
		t.Fatalf("StoredBytes = %d, want >= 1500 (text + signature + redacted)", got)
	}
	if got := st.ReasoningText(); len(got) != len("short thought") {
		t.Fatalf("ReasoningText len = %d, want %d", len(got), len("short thought"))
	}

	// Total budget: a 100-byte total cap must refuse the second
	// signature-heavy entry even though each text view is tiny.
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntries: 100, MaxTotalBytes: 1100})
	c.SetStateForTenant(ctx, ToolCallKey("t", "call_1"), "t", "", "m", st)
	if got := c.Stats().Bytes; got < 1500 {
		t.Fatalf("Stats().Bytes = %d, want >= 1500 (signature + redacted counted)", got)
	}
	// The entry itself is 1500+ bytes, so with a 1100-byte total
	// cap the store funnel keeps it only as the single entry
	// (eviction cannot shrink below one entry) — but a second
	// entry must evict the first: the budget is genuinely
	// enforced, not text-only.
	c.SetStateForTenant(ctx, ToolCallKey("t", "call_2"), "t", "", "m", st)
	if c.Size() != 1 {
		t.Fatalf("Size = %d after second store, want 1 (total cap evicts)", c.Size())
	}
	if _, ok := c.Get(ctx, ToolCallKey("t", "call_1")); ok {
		t.Errorf("first entry still present after second store over the total cap")
	}

	// Per-entry cap: a state whose STORED bytes exceed the cap is
	// refused even when its text is short.
	c2 := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntryBytes: 100})
	c2.SetStateForTenant(ctx, ToolCallKey("t", "call_1"), "t", "", "m", st)
	if c2.Size() != 0 {
		t.Errorf("signature-heavy state stored despite per-entry cap, Size = %d", c2.Size())
	}
	// And a text-only state of the same short text still stores.
	c2.SetStateForTenant(ctx, ToolCallKey("t", "call_2"), "t", "", "m", reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "short thought"}},
	})
	if c2.Size() != 1 {
		t.Errorf("text-only state refused, Size = %d, want 1", c2.Size())
	}
}

func TestG13ReasoningDeleteAndSyncDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reasoning.db")
	c, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentReasoningCache: %v", err)
	}
	key := ToolCallKey("tenant-1", "call_7")
	c.SetStateForTenant(ctx, key, "tenant-1", "", "m", reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "durable thought"}},
	})
	// Delete must be durable: a restart must not resurrect it.
	c.Delete(ctx, key)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c2, err := OpenPersistentReasoningCache(path, ReasoningCacheConfig{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer c2.Close()
	if _, ok := c2.GetState(ctx, key); ok {
		t.Errorf("deleted entry resurrected after reload")
	}
	// Sync on a persistent cache writes the live entries; a
	// memory-only cache reports 0, nil (nothing to sync).
	if n, err := c2.Sync(ctx); err != nil || n != 0 {
		t.Errorf("Sync on empty persistent cache = (%d, %v), want (0, nil)", n, err)
	}
	mem := NewReasoningCache()
	if n, err := mem.Sync(ctx); err != nil || n != 0 {
		t.Errorf("Sync on memory-only cache = (%d, %v), want (0, nil)", n, err)
	}
	if err := mem.Close(); err != nil {
		t.Errorf("Close on memory-only cache: %v", err)
	}
}
