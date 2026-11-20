package router

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// G22 wire test (parity tracker Area 10 row :868): the engine's
// compression decision is a production caller of
// compress.ApplyWithOptions — the cache-aware inputs must come
// from real signals, not zero options.
//
// A request naming mode "aggressive" against an engine whose
// configured providers include a prompt-cache-marker provider
// (anthropic) is downgraded to "standard", so a rewriting mode
// cannot break the cacheable prefix (the reference router #3955: the
// provider alone is sufficient — no cache_control marker is
// required). Against an engine with no caching provider the
// mode stands. This test FAILS while maybeCompress calls plain
// compress.Apply (zero options): both engines keep "aggressive".
func TestMaybeCompressDowngradesAggressiveForCachingProvider(t *testing.T) {
	msgs := []provider.Message{
		{Role: "system", Content: strings.Repeat("The system instruction is to answer clearly and completely with citations for every claim made. ", 40)},
		{Role: "user", Content: strings.Repeat("Please keep the answer short and focused on the question asked. ", 12)},
	}
	newReq := func() provider.ChatRequest {
		return provider.ChatRequest{
			Model:    "gpt-4o",
			Messages: msgs,
			Compression: &provider.CompressionRequest{
				Lossless:             boolPtr(true),
				LossyMaxPromptTokens: 10000,
				Mode:                 "aggressive",
			},
		}
	}

	caching := NewEngine(map[string]provider.Client{"anthropic": okClient("hi")}, &fakeLedger{}, Config{})
	out := caching.maybeCompress(newReq())
	if out.Compressed == nil {
		t.Fatalf("caching engine: no compression stamp, want one with Mode standard")
	}
	if out.Compressed.Mode != "standard" {
		t.Fatalf("caching engine: Mode = %q, want %q (aggressive must downgrade for a caching provider)", out.Compressed.Mode, "standard")
	}

	plain := NewEngine(map[string]provider.Client{"openai": okClient("hi")}, &fakeLedger{}, Config{})
	out = plain.maybeCompress(newReq())
	if out.Compressed == nil {
		t.Fatalf("plain engine: no compression stamp, want one with Mode aggressive")
	}
	if out.Compressed.Mode != "aggressive" {
		t.Fatalf("plain engine: Mode = %q, want %q (no caching provider, no downgrade)", out.Compressed.Mode, "aggressive")
	}
}
