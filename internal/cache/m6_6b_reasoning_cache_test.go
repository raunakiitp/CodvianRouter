package cache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/reasoning"
)

// M6.6b — the reasoning cache stores the FULL reasoning state,
// not text only (the M6.6 text-only store shadowed the router's
// signed state). Get still returns the text view; GetState
// returns the cloned full state. Toggle evidence: with
// SetStateForTenant storing text only, the signature assertions
// below fail.

func TestM66bStateRoundTripPreservesSignature(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	st := reasoning.State{
		Blocks: []reasoning.Block{{
			Kind:      reasoning.KindThinking,
			Text:      "anthropic thought",
			Signature: "sig-abc-123",
		}},
		SourceWire:  reasoning.WireAnthropic,
		SourceModel: "claude-sonnet-4-5",
	}
	c.SetStateForTenant(ctx, key, "tenant-a", "", "claude-sonnet-4-5", st)

	// The text view is unchanged.
	if got, ok := c.Get(ctx, key); !ok || got != "anthropic thought" {
		t.Fatalf("Get = %q, %v, want ('anthropic thought', true)", got, ok)
	}
	// The full state survives verbatim.
	got, ok := c.GetState(ctx, key)
	if !ok {
		t.Fatalf("GetState missed a stored state")
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindThinking ||
		got.Blocks[0].Text != "anthropic thought" || got.Blocks[0].Signature != "sig-abc-123" {
		t.Fatalf("GetState = %+v, want one signed KindThinking block", got.Blocks)
	}
	if got.SourceWire != reasoning.WireAnthropic || got.SourceModel != "claude-sonnet-4-5" {
		t.Fatalf("GetState provenance = (%q, %q), want (anthropic, claude-sonnet-4-5)", got.SourceWire, got.SourceModel)
	}
	// The returned state is a clone: mutating it must not mutate
	// the stored entry.
	got.Blocks[0].Text = "tampered"
	got.Blocks[0].Signature = "tampered"
	again, ok := c.GetState(ctx, key)
	if !ok || again.Blocks[0].Text != "anthropic thought" || again.Blocks[0].Signature != "sig-abc-123" {
		t.Fatalf("stored state mutated through a returned clone: %+v", again.Blocks)
	}
	// Storing must not mutate the caller's state either (clone
	// on the way in).
	st.Blocks[0].Text = "caller changed"
	third, _ := c.GetState(ctx, key)
	if third.Blocks[0].Text != "anthropic thought" {
		t.Fatalf("stored state mutated through the caller's state: %q", third.Blocks[0].Text)
	}
}

func TestM66bStateRoundTripPreservesBedrockData(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	data := json.RawMessage(`{"reasoningContent":{"reasoningText":{"text":"bedrock thought","signature":"bedrock-sig-9"}}}`)
	c.SetState(ctx, key, "", "anthropic.claude-sonnet-4-5", reasoning.State{
		Blocks:     []reasoning.Block{{Kind: reasoning.KindBedrock, Data: data}},
		SourceWire: reasoning.WireBedrock,
	})
	got, ok := c.GetState(ctx, key)
	if !ok || len(got.Blocks) != 1 || string(got.Blocks[0].Data) != string(data) {
		t.Fatalf("GetState = %v, %v, want the Bedrock Data verbatim", got.Blocks, ok)
	}
	if text, ok := c.Get(ctx, key); !ok || text != "bedrock thought" {
		t.Fatalf("Get (text view of a Bedrock block) = %q, %v, want ('bedrock thought', true)", text, ok)
	}
}

func TestM66bTextSetYieldsThoughtState(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	c.Set(ctx, key, "deepseek", "deepseek-chat", "thinking about math")
	got, ok := c.GetState(ctx, key)
	if !ok {
		t.Fatalf("GetState missed a text-only entry")
	}
	if len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindThought || got.Blocks[0].Text != "thinking about math" {
		t.Fatalf("GetState after text Set = %+v, want one KindThought block with the text", got.Blocks)
	}
	if got.Blocks[0].Signature != "" {
		t.Fatalf("text-only entry carries a signature %q, want none (never invented)", got.Blocks[0].Signature)
	}
}

func TestM66bStateTextCapStillRefuses(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCacheWithConfig(ReasoningCacheConfig{MaxEntryBytes: 10})
	key := ToolCallKey("tenant-a", "call_1")
	// G13: the per-entry cap applies to the STORED size
	// (text + signature + Data), not the text alone — the
	// signature is part of what the entry holds. A state with
	// 11 text + 3 signature bytes (14 stored) is refused.
	over := reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: strings.Repeat("x", 11), Signature: "sig"}}}
	c.SetState(ctx, key, "", "m", over)
	if _, ok := c.GetState(ctx, key); ok {
		t.Fatalf("oversize state stored, want refused (M6.1 per-entry cap applies to the stored size)")
	}
	// At exactly the cap in STORED bytes (7 text + 3 signature
	// = 10) the state is stored, signature and all.
	atCap := reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: strings.Repeat("x", 7), Signature: "sig"}}}
	c.SetState(ctx, key, "", "m", atCap)
	if got, ok := c.GetState(ctx, key); !ok || got.Blocks[0].Signature != "sig" {
		t.Fatalf("at-cap state not stored with its signature, want stored: %v, %v", got.Blocks, ok)
	}
}

func TestM66bRedactedOnlyStateStores(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	c.SetState(ctx, key, "", "claude-sonnet-4-5", reasoning.State{
		Blocks: []reasoning.Block{{Kind: reasoning.KindRedactedThinking, Data: json.RawMessage(`"encrypted-blob"`)}},
	})
	got, ok := c.GetState(ctx, key)
	if !ok || len(got.Blocks) != 1 || got.Blocks[0].Kind != reasoning.KindRedactedThinking {
		t.Fatalf("GetState = %v, %v, want the redacted-only state (no text, still stored)", got.Blocks, ok)
	}
	// No text view — but the entry exists.
	if text, ok := c.Get(ctx, key); !ok || text != "" {
		t.Fatalf("Get = %q, %v, want ('', true) (entry exists, no text view)", text, ok)
	}
}

func TestM66bEmptyStateRefused(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	c.SetState(ctx, key, "", "m", reasoning.State{})
	if _, ok := c.GetState(ctx, key); ok {
		t.Fatalf("empty state stored, want refused")
	}
	c.SetState(ctx, "", "", "m", reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThought, Text: "x"}}})
	if c.Size() != 0 {
		t.Fatalf("Size = %d after empty-key store, want 0", c.Size())
	}
}

func TestM66bStateReplaceRefreshesWithoutGrowing(t *testing.T) {
	ctx := context.Background()
	c := NewReasoningCache()
	key := ToolCallKey("tenant-a", "call_1")
	c.SetState(ctx, key, "", "m", reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "v1", Signature: "s1"}}})
	c.SetState(ctx, key, "", "m", reasoning.State{Blocks: []reasoning.Block{{Kind: reasoning.KindThinking, Text: "v2", Signature: "s2"}}})
	if c.Size() != 1 {
		t.Fatalf("Size = %d after replace, want 1", c.Size())
	}
	got, _ := c.GetState(ctx, key)
	if got.Blocks[0].Text != "v2" || got.Blocks[0].Signature != "s2" {
		t.Fatalf("replaced state = %+v, want v2/s2", got.Blocks)
	}
}
