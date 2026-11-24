// P1.5 capability gate on the direct path: when the caller requires
// capabilities (Config.RequireCapabilities), an attempt must hold each
// one with VERIFIED support. Unknown — including a provider/model the
// matrix never heard of — never satisfies a requirement, unlike the
// CT-1 tool pre-check which fails open for plain chat traffic.
package router

import (
	"context"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/capability"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// capabilityLookupFrom builds a Config.CapabilityLookup from a
// providerID -> ModelCapabilities table (model id ignored).
func capabilityLookupFrom(table map[string]capability.ModelCapabilities) func(string, string) (capability.ModelCapabilities, bool) {
	return func(providerID, _ string) (capability.ModelCapabilities, bool) {
		mc, ok := table[providerID]
		return mc, ok
	}
}

func verifiedVision() capability.ModelCapabilities {
	return capability.ModelCapabilities{
		capability.CapVision: {
			Capability: capability.CapVision,
			Support:    capability.SupportSupported,
			Provenance: capability.Provenance{Source: capability.SourceModelsDevSync, Verification: capability.VerificationVerified},
		},
	}
}

func unverifiedVision() capability.ModelCapabilities {
	return capability.ModelCapabilities{
		capability.CapVision: {
			Capability: capability.CapVision,
			Support:    capability.SupportSupported,
			Provenance: capability.Provenance{Source: capability.SourceOperatorConfig, Verification: capability.VerificationUnverified},
		},
	}
}

// TestP15CapabilityGateSkipsUnverified: a declared-but-unverified model
// and a model absent from the matrix are both skipped; the verified
// target serves.
func TestP15CapabilityGateSkipsUnverified(t *testing.T) {
	ctx := context.Background()
	declared := errClient(&provider.UpstreamError{StatusCode: 500, Body: "must never be dialled"})
	unknown := errClient(&provider.UpstreamError{StatusCode: 500, Body: "must never be dialled"})
	known := okClient("ok")
	eng := NewEngine(map[string]provider.Client{"declared": declared, "unknown": unknown, "known": known}, nil, Config{
		HedgeDelay:          -1,
		RequireCapabilities: []capability.Capability{capability.CapVision},
		CapabilityLookup: capabilityLookupFrom(map[string]capability.ModelCapabilities{
			"declared": unverifiedVision(),
			"known":    verifiedVision(),
			// "unknown" intentionally absent: no evidence at all.
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("declared", "unknown", "known"), coolReq("m"))
	if err != nil || resp.Content != "ok" {
		t.Fatalf("chat = %v, %v, want ok + nil", resp, err)
	}
	if declared.chatCalls() != 0 || unknown.chatCalls() != 0 {
		t.Fatalf("unverified attempts dialled: declared=%d unknown=%d, want 0/0", declared.chatCalls(), unknown.chatCalls())
	}
	if known.chatCalls() != 1 {
		t.Fatalf("verified attempt called %d times, want 1", known.chatCalls())
	}
}

// TestP15CapabilityGateNoVerifiedAttempt: when nothing in the chain is
// verified for the required capability, the request fails with
// ErrNoAvailableAttempt instead of dialling an unverified model.
func TestP15CapabilityGateNoVerifiedAttempt(t *testing.T) {
	ctx := context.Background()
	a := errClient(&provider.UpstreamError{StatusCode: 500, Body: "must never be dialled"})
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay:          -1,
		RequireCapabilities: []capability.Capability{capability.CapVision},
		CapabilityLookup: capabilityLookupFrom(map[string]capability.ModelCapabilities{
			"a": unverifiedVision(),
		}),
	})
	_, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != ErrNoAvailableAttempt {
		t.Fatalf("err = %v, want ErrNoAvailableAttempt", err)
	}
	if got := a.chatCalls(); got != 0 {
		t.Fatalf("unverified provider called %d times, want 0", got)
	}
}

// TestP15NoRequirementKeepsDefaultRouting: an empty
// RequireCapabilities list leaves the chain untouched even when the
// lookup says nothing is verified — default behaviour must not change.
func TestP15NoRequirementKeepsDefaultRouting(t *testing.T) {
	ctx := context.Background()
	a := okClient("aye")
	eng := NewEngine(map[string]provider.Client{"a": a}, nil, Config{
		HedgeDelay: -1,
		CapabilityLookup: capabilityLookupFrom(map[string]capability.ModelCapabilities{
			"a": unverifiedVision(),
		}),
	})
	resp, _, err := eng.ChatWithChain(ctx, chainFor("a"), coolReq("m"))
	if err != nil || resp.Content != "aye" {
		t.Fatalf("chat = %v, %v, want aye + nil", resp, err)
	}
	if got := a.chatCalls(); got != 1 {
		t.Fatalf("provider A called %d times, want 1", got)
	}
}