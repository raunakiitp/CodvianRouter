// Reversible PII token vault, server plumbing (B3-19,
// PARITY.md:782): the request rewrite mints tokens and the
// response restore swaps them back. The full HTTP path is
// proven by the V lane on the PC; these tests pin the two pure
// plumbing functions so a refactor cannot silently drop the
// mint or the restore.
package server

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
	"github.com/raunakiitp/codvian-router/internal/guardrails"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// testPIIVault builds a memory-only vault sealing under a
// throwaway master secret (same shape as production, where the
// sealer is the router's crypto.Vault).
func testPIIVault(t *testing.T) *guardrails.Vault {
	t.Helper()
	cv, err := crypto.NewFromSecret([]byte("server-vault-test-secret-32bytes!"), t.TempDir())
	if err != nil {
		t.Fatalf("crypto vault: %v", err)
	}
	return guardrails.NewVault(cv, "")
}

func TestRewriteRequestWithVaultMintsAndRestores(t *testing.T) {
	v := testPIIVault(t)
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "mail alice@example.com, call +1 415-555-0132"}}}
	minted := rewriteRequestWithVault(&req, v)
	if minted != 2 {
		t.Fatalf("minted = %d, want 2 (email + phone)", minted)
	}
	content := req.Messages[0].Content
	if strings.Contains(content, "alice@example.com") || strings.Contains(content, "415-555-0132") {
		t.Fatalf("rewritten request still contains PII: %q", content)
	}
	tok := guardrails.TokenRegexp.FindString(content)
	if tok == "" {
		t.Fatalf("rewritten request has no vault token: %q", content)
	}
	resp := provider.ChatResponse{Content: "I will mail " + tok + " today"}
	if n := restoreVaultTokens(&resp, v); n != 1 || resp.Content != "I will mail alice@example.com today" {
		t.Fatalf("restore = (%q, %d), want original back, 1", resp.Content, n)
	}
}

func TestRewriteRequestWithVaultNilVaultIsPlainRewrite(t *testing.T) {
	req := provider.ChatRequest{Model: "m", Messages: []provider.Message{{Role: "user", Content: "mail alice@example.com"}}}
	if minted := rewriteRequestWithVault(&req, nil); minted != 0 {
		t.Fatalf("minted = %d with nil vault, want 0", minted)
	}
	if req.Messages[0].Content != "mail [REDACTED:EMAIL]" {
		t.Fatalf("nil-vault rewrite = %q, want plain placeholder", req.Messages[0].Content)
	}
	resp := provider.ChatResponse{Content: "hi"}
	if n := restoreVaultTokens(&resp, nil); n != 0 || resp.Content != "hi" {
		t.Fatalf("nil-vault restore = (%q, %d), want unchanged, 0", resp.Content, n)
	}
}

func TestRestoreVaultTokensToolCalls(t *testing.T) {
	v := testPIIVault(t)
	tok, err := v.Mint("email", "bob@example.com")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	resp := provider.ChatResponse{
		Content: "done",
		ToolCalls: []provider.ToolCall{{
			Function: provider.ToolCallFunction{Name: "send", Arguments: `{"to": "` + tok + `"}`},
		}},
	}
	if n := restoreVaultTokens(&resp, v); n != 1 {
		t.Fatalf("restored = %d, want 1", n)
	}
	if resp.ToolCalls[0].Function.Arguments != `{"to": "bob@example.com"}` {
		t.Fatalf("tool-call arguments = %q, want original back", resp.ToolCalls[0].Function.Arguments)
	}
}
