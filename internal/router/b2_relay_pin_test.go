// Package router — B2 context-relay pin tests (PARITY row :489).
//
// V-B2-489's verdict: the three :489 producers (commit d3abe14) are
// real and in the request path, but NO producer is pinned by any
// committed test — the existing relay tests inject fake summarisers
// (they pin the engine seam only) and never drive two requests under
// one CallerID. These tests pin the third producer, the cross-request
// session store (relay.go: storeRelayWantIfPending / loadRelayWant /
// newRelayFor preload), through the public interface only:
//
//   - request 1 (codex, success with quota use 0.90 in the relay
//     window) ends in terminal success: no handoff is ever built
//     (the lazy build only runs when another attempt is dialled), so
//     the want must cross into the next request via the store;
//   - request 2 (same CallerID) preloads the want, builds the
//     (degraded — no summariser configured) handoff and injects it;
//   - request 3 (same CallerID) gets nothing: the want is consumed
//     exactly once;
//   - a second test proves the want is keyed by CallerID: a request
//     under another key never sees it, and the original key still
//     does afterwards.
//
// With the store disabled (storeRelayWantIfPending / loadRelayWant
// removed), request 2 gets no handoff and both tests FAIL. The
// production summariser and quota signal are pinned through the real
// stack in cmd/codvian-router/b2_relay_pin_test.go — this file pins
// the store in isolation, with the quota signal stubbed at the
// Config seam (RelayQuotaUsedFor) so a store failure cannot hide
// behind a signal failure.
package router

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestB2RelayPinCrossRequestWantStoredAndPreloaded pins the
// cross-request session store: a success-triggered want outlives its
// request, is preloaded by the next request under the same CallerID,
// and is consumed exactly once.
func TestB2RelayPinCrossRequestWantStoredAndPreloaded(t *testing.T) {
	codex := okClient("from-codex")
	other := okClient("from-other")
	cfg := testConfig()
	// Quota signal stubbed at the Config seam: codex is 90% used
	// (in the [0.85, 0.95) relay window), everything else unknown.
	cfg.RelayQuotaUsedFor = func(providerID, model string) (float64, bool) {
		if providerID == "codex" {
			return 0.9, true
		}
		return 0, false
	}
	eng := NewEngine(map[string]provider.Client{"codex": codex, "other": other}, nil, cfg)

	// Request 1: codex succeeds. The outcome is recorded (want set)
	// but no handoff is built — a terminal success has no next
	// attempt to carry it to, and the lazy build never runs.
	req1 := relayReq()
	req1.CallerID = "relay-pin-key-1"
	if resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex"), req1, "context-relay"); err != nil {
		t.Fatalf("request 1: %v", err)
	} else if resp.Content != "from-codex" {
		t.Fatalf("request 1 content = %q, want from-codex", resp.Content)
	}
	st, ok := eng.LastRelayStatus()
	if !ok {
		t.Fatal("LastRelayStatus ok=false after request 1, want the recorded outcome")
	}
	if st.Attempts != 1 {
		t.Fatalf("request 1 relay attempts = %d, want 1", st.Attempts)
	}
	if st.HandoffActive {
		t.Fatal("request 1 handoff active, want none (terminal success builds nothing)")
	}

	// Request 2, same key: the stored want is preloaded, the
	// handoff is built (degraded — no summariser configured) and
	// injected as the leading system message of other's request.
	req2 := relayReq()
	req2.CallerID = "relay-pin-key-1"
	if resp, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("other"), req2, "context-relay"); err != nil {
		t.Fatalf("request 2: %v", err)
	} else if resp.Content != "from-other" {
		t.Fatalf("request 2 content = %q, want from-other", resp.Content)
	}
	req, ok := lastChatReq(other)
	if !ok {
		t.Fatal("other client received no chat on request 2")
	}
	handoff := firstHandoff(req)
	if handoff == "" {
		t.Fatalf("request 2 got no handoff; messages = %+v", req.Messages)
	}
	if !strings.Contains(handoff, "Unsummarised transcript") {
		t.Fatalf("request 2 handoff lacks the unsummarised marker: %q", handoff)
	}
	if !strings.Contains(handoff, "what is 2+2?") {
		t.Fatalf("request 2 handoff does not carry the raw transcript: %q", handoff)
	}
	st, _ = eng.LastRelayStatus()
	if !st.HandoffActive || st.HandoffSummarised {
		t.Fatalf("request 2 relay status active=%v summarised=%v, want active=true summarised=false", st.HandoffActive, st.HandoffSummarised)
	}
	if st.DegradeReason != "no summariser configured" {
		t.Fatalf("request 2 degrade reason = %q, want %q", st.DegradeReason, "no summariser configured")
	}

	// Request 3, same key: the want was consumed exactly once —
	// a third request starts cold.
	req3 := relayReq()
	req3.CallerID = "relay-pin-key-1"
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("other"), req3, "context-relay"); err != nil {
		t.Fatalf("request 3: %v", err)
	}
	req, _ = lastChatReq(other)
	if got := firstHandoff(req); got != "" {
		t.Fatalf("request 3 got a handoff for a consumed want: %q", got)
	}
}

// TestB2RelayPinWantKeyedByCallerID pins the store's keying: a want
// stored under one gateway key is invisible to a request under
// another key, and still waiting for the original key afterwards.
func TestB2RelayPinWantKeyedByCallerID(t *testing.T) {
	codex := okClient("from-codex")
	other := okClient("from-other")
	cfg := testConfig()
	cfg.RelayQuotaUsedFor = func(providerID, model string) (float64, bool) {
		if providerID == "codex" {
			return 0.9, true
		}
		return 0, false
	}
	eng := NewEngine(map[string]provider.Client{"codex": codex, "other": other}, nil, cfg)

	// Request 1 under key-A: codex succeeds, the want is stored
	// under key-A only.
	req1 := relayReq()
	req1.CallerID = "relay-pin-key-A"
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("codex"), req1, "context-relay"); err != nil {
		t.Fatalf("request 1 (key-A): %v", err)
	}

	// Request 2 under key-B: no preloaded want, no handoff.
	req2 := relayReq()
	req2.CallerID = "relay-pin-key-B"
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("other"), req2, "context-relay"); err != nil {
		t.Fatalf("request 2 (key-B): %v", err)
	}
	req, ok := lastChatReq(other)
	if !ok {
		t.Fatal("other client received no chat on request 2 (key-B)")
	}
	if got := firstHandoff(req); got != "" {
		t.Fatalf("request 2 (key-B) got a handoff stored under key-A: %q", got)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("request 2 (key-B) messages = %d, want 2 (originals only)", len(req.Messages))
	}

	// Request 3 under key-A: the want is still there — key-B's
	// request never consumed it.
	req3 := relayReq()
	req3.CallerID = "relay-pin-key-A"
	if _, _, err := eng.ChatWithChainStrategy(context.Background(), relayChain("other"), req3, "context-relay"); err != nil {
		t.Fatalf("request 3 (key-A): %v", err)
	}
	req, _ = lastChatReq(other)
	if handoff := firstHandoff(req); handoff == "" {
		t.Fatalf("request 3 (key-A) got no handoff; messages = %+v", req.Messages)
	} else if !strings.Contains(handoff, "what is 2+2?") {
		t.Fatalf("request 3 (key-A) handoff does not carry the raw transcript: %q", handoff)
	}
}
