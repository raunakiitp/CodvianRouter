package capability

// p2_capability_closure_test.go — audit findings P2-PROV-001 and P2-PROV-002.
//
// P2-PROV-001: the catalogue's executable matrix claimed emb/rerank/images/
// audio for wires the runtime refuses (gemini, vertex, cohere). The matrix
// and the router gates now derive from one rule; this test fails if the two
// copies ever diverge.
//
// P2-PROV-002: eight capability names were accepted by config validation but
// never populated by any builder, so require_capabilities silently skipped
// every attempt. Four (the api_* ones) are now populated from the wire rule;
// the other four are rejected at config load. This test iterates ALL 19 names
// and asserts each is either usable or explicitly rejected with a reason.

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestPassthroughRuleMatchesRuntime is the guard that keeps the catalogue
// matrix and the router's selection gates in agreement.
func TestPassthroughRuleMatchesRuntime(t *testing.T) {
	for _, w := range []string{"", "openai", "anthropic", "gemini", "vertex",
		"cohere", "bedrock-converse", "ollama", "unknown"} {
		if got, want := catalog.EndpointPassthroughSupported(w), provider.WireFormatServesPassthrough(w); got != want {
			t.Errorf("wire %q: catalogue rule = %v, runtime rule = %v (the matrix and the router disagree)", w, got, want)
		}
	}
}

// TestAPICapabilitiesPopulatedFromWire proves the four endpoint capabilities
// are recorded (never silently absent) and that the recorded support matches
// what the router would actually select.
func TestAPICapabilitiesPopulatedFromWire(t *testing.T) {
	cases := []struct {
		wire catalog.WireFormat
		want Support
	}{
		{catalog.WireOpenAI, SupportSupported},
		{catalog.WireGemini, SupportUnsupported},
		{catalog.WireAnthropic, SupportUnsupported},
		{catalog.WireCohere, SupportUnsupported},
		{catalog.WireBedrock, SupportUnsupported},
	}
	for _, tc := range cases {
		b := &Builder{}
		out := map[string]ModelCapabilities{}
		cp := &catalog.Provider{ID: "p", WireFormats: []catalog.WireFormat{tc.wire}}
		b.buildModels(out, "p", cp, nil, []string{"m"})
		mc, ok := out["p|m"]
		if !ok {
			t.Fatalf("wire %s: no capabilities built", tc.wire)
		}
		for _, c := range []Capability{CapAPIEmbeddings, CapAPIRerank, CapAPIImages, CapAPIAudio} {
			e, present := mc[c]
			if !present {
				t.Errorf("wire %s: %s is absent — the filter would silently skip every attempt", tc.wire, c)
				continue
			}
			if e.Support != tc.want {
				t.Errorf("wire %s: %s support = %s, want %s", tc.wire, c, e.Support, tc.want)
			}
			if e.Provenance.Source == "" {
				t.Errorf("wire %s: %s has no provenance", tc.wire, c)
			}
		}
	}
}

// TestAllCapabilityNamesAreUsableOrRejected covers all 19 names: each must
// parse, and must be either usable in config or rejected with a non-empty
// reason. Exactly the four unpoulatable ones are rejected.
func TestAllCapabilityNamesAreUsableOrRejected(t *testing.T) {
	rejected := map[Capability]string{}
	names := KnownCapabilities()
	if len(names) != 19 {
		t.Fatalf("KnownCapabilities = %d names, want 19", len(names))
	}
	for _, name := range names {
		c, ok := ParseCapability(name)
		if !ok {
			t.Fatalf("%q does not parse", name)
		}
		reason, usable := Configurable(c)
		if !usable {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%q rejected with an empty reason", name)
			}
			rejected[c] = reason
		}
	}
	want := map[Capability]bool{
		CapImageOutput: true, CapAudioOutput: true, CapStructuredOutput: true, CapJSONMode: true,
	}
	if len(rejected) != len(want) {
		t.Fatalf("rejected %d capabilities, want %d: %v", len(rejected), len(want), rejected)
	}
	for c := range want {
		if _, ok := rejected[c]; !ok {
			t.Errorf("%s should be rejected at config load (nothing populates it)", c)
		}
	}
	for c := range rejected {
		if !want[c] {
			t.Errorf("%s is rejected but nothing in this test says why it must be", c)
		}
	}
}
