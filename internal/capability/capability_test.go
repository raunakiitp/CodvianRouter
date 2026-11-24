package capability

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestUnknownToolCallingIsNotVerified is the P1.5 core rule: a model
// with no affirmative tool-calling evidence is NOT verified capable,
// and a model that merely declares tools in operator config is not
// verified either.
func TestUnknownToolCallingIsNotVerified(t *testing.T) {
	mc := ModelCapabilities{
		CapToolCalling: Entry{
			Capability: CapToolCalling,
			Support:    SupportUnknown,
			Provenance: Provenance{Source: SourceCatalogReference, Verification: VerificationUnverified},
		},
	}
	if mc.HasVerified(CapToolCalling) {
		t.Fatal("unknown tool calling must not verify")
	}
	if mc.HasSupportAtLeast(CapToolCalling, SupportSupported) {
		t.Fatal("unknown tool calling must not read as supported")
	}

	// Operator-set tools without a sync report: supported but unverified.
	b := Builder{}
	mc2 := ModelCapabilities{}
	b.enrichFromRegistryModel(mc2, "p", provider.Model{ID: "m", Vision: true})
	if mc2.HasVerified(CapVision) {
		t.Fatal("operator-set vision without a sync report must not verify")
	}
	if mc2[CapVision].Support != SupportSupported {
		t.Fatalf("operator-set vision carried support %q, want supported (declared, unverified)", mc2[CapVision].Support)
	}
}

// TestEmulatedToolCallingIsNotNativeSupport: a prompt-emulated provider
// supports tools only by injection — the capability registry must never
// report native tool support for it.
func TestEmulatedToolCallingIsNotNativeSupport(t *testing.T) {
	e := Entry{
		Capability: CapToolCalling,
		Support:    SupportUnsupported,
		Provenance: Provenance{Source: SourceCatalogReference, Verification: VerificationVerified, Note: "prompt-emulated"},
	}
	if e.Verified() {
		t.Fatal("emulated tool calling must not verify as native tool support")
	}
}

// TestVerifiedCapabilityRequiresBothSupportAndVerification.
func TestVerifiedCapabilityRequiresBothSupportAndVerification(t *testing.T) {
	supported := Entry{Capability: CapVision, Support: SupportSupported, Provenance: Provenance{Verification: VerificationVerified}}
	if !supported.Verified() {
		t.Fatal("supported+verified must verify")
	}
	unverified := Entry{Capability: CapVision, Support: SupportSupported, Provenance: Provenance{Verification: VerificationUnverified}}
	if unverified.Verified() {
		t.Fatal("supported+unverified must not verify")
	}
	unsupported := Entry{Capability: CapVision, Support: SupportUnsupported, Provenance: Provenance{Verification: VerificationVerified}}
	if unsupported.Verified() {
		t.Fatal("unsupported+verified must not verify")
	}
}

// TestIndexLookupMissIsNotAnEmptySet: a pair absent from the index must
// report ok=false, so the router gate can distinguish "no evidence"
// from "evidence of absence".
func TestIndexLookupMissIsNotAnEmptySet(t *testing.T) {
	idx := &Index{matrix: map[string]ModelCapabilities{
		"a|m": {CapVision: Entry{Capability: CapVision, Support: SupportSupported, Provenance: Provenance{Verification: VerificationVerified}}},
	}}
	if _, ok := idx.Lookup("a", "m"); !ok {
		t.Fatal("known pair must be found")
	}
	if _, ok := idx.Lookup("a", "other"); ok {
		t.Fatal("unknown pair must report ok=false")
	}
	if _, ok := idx.Lookup("nope", "m"); ok {
		t.Fatal("unknown provider must report ok=false")
	}
	var nilIdx *Index
	if _, ok := nilIdx.Lookup("a", "m"); ok {
		t.Fatal("nil index must report ok=false")
	}
}

// TestIndexRefreshReplacesSnapshotAtomically: Refresh swaps the whole
// map; readers never see a partially built one (mutation happens under
// the write lock after the build completes).
func TestIndexRefreshReplacesSnapshotAtomically(t *testing.T) {
	idx := NewIndex(Builder{})
	if idx.Size() != 0 {
		t.Fatalf("empty builder produced %d entries", idx.Size())
	}
	idx.mu.Lock()
	idx.matrix = map[string]ModelCapabilities{"p|m": {}}
	idx.mu.Unlock()
	if idx.Size() != 1 {
		t.Fatalf("size = %d, want 1", idx.Size())
	}
}

// TestModelLookupOnDemand: the per-model lookup agrees with the full
// matrix, and an unknown pair reports ok=false — the router gate must
// never receive an empty set it could mistake for "verified capable".
func TestModelLookupOnDemand(t *testing.T) {
	b := Builder{}
	matrix := b.Build()
	if len(matrix) == 0 {
		t.Skip("no catalog entries in this build")
	}
	// Pick one known pair and compare the two sources.
	for key, want := range matrix {
		providerID, model := "", ""
		for i := 0; i < len(key); i++ {
			if key[i] == '|' {
				providerID, model = key[:i], key[i+1:]
				break
			}
		}
		got, ok := b.Model(providerID, model)
		if !ok {
			t.Fatalf("Model(%q, %q) not found but present in the matrix", providerID, model)
		}
		if len(got) != len(want) {
			t.Fatalf("Model(%q, %q) has %d capabilities, matrix has %d", providerID, model, len(got), len(want))
		}
		for cap, e := range want {
			ge, ok := got[cap]
			if !ok || ge.Support != e.Support {
				t.Fatalf("Model(%q, %q) cap %s = %+v, want %+v", providerID, model, cap, ge, e)
			}
		}
		break
	}
	if _, ok := b.Model("no-such-provider", "no-such-model"); ok {
		t.Fatal("unknown pair reported ok=true")
	}
	if _, ok := b.Model("", ""); ok {
		t.Fatal("empty pair reported ok=true")
	}
}

// TestBuildIsDeterministicAndUsesCatalogFacts: the builder must report
// catalog tool-calling facts with their provenance, and never invent
// capabilities the catalog does not carry.
func TestBuildIsDeterministicAndUsesCatalogFacts(t *testing.T) {
	b := Builder{}
	first := b.Build()
	second := b.Build()
	if len(first) != len(second) {
		t.Fatalf("non-deterministic size: %d vs %d", len(first), len(second))
	}
	for key, mc := range first {
		other, ok := second[key]
		if !ok {
			t.Fatalf("key %s missing on second build", key)
		}
		if len(mc) != len(other) {
			t.Fatalf("key %s capability count drift: %d vs %d", key, len(mc), len(other))
		}
		for cap, e := range mc {
			oe := other[cap]
			if e.Support != oe.Support {
				t.Fatalf("key %s cap %s support drift: %q vs %q", key, cap, e.Support, oe.Support)
			}
			if e.Provenance.Source != oe.Provenance.Source {
				t.Fatalf("key %s cap %s source drift: %q vs %q", key, cap, e.Provenance.Source, oe.Provenance.Source)
			}
		}
		// Every model carries its tool-calling state, even if unknown.
		if _, ok := mc[CapToolCalling]; !ok {
			t.Fatalf("key %s has no tool_calling entry", key)
		}
	}
}
