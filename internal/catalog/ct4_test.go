package catalog

import "testing"

// CT-4 (GAP-TRACK entry 403): keyless no-auth providers were copied
// without the ToS-avoid flag or any emulated-tool-calling marker.
// the reference router marks four of them toolCalling "emulated" (noauth.ts) and
// states tool calling is unavailable for the fifth (aihorde); all five
// are flagged ToSGrey and none counts as tool-capable.

// ct4Entry403ToSGrey are the five entry-403 providers: verified against
// the the reference router reference (src/shared/constants/providers/noauth.ts)
// by reading each entry. Four carry toolCalling "emulated"; aihorde
// carries no toolCalling marker and its notice states tool calling is
// unavailable — non-native either way, so it belongs to the set.
var ct4Entry403ToSGrey = []string{
	"devin-cli-agentic",
	"duckduckgo-web",
	"cloudflare-playground",
	"uncloseai",
	"aihorde",
}

// ct4Emulated are the no-auth entries the reference marks
// toolCalling "emulated" (noauth.ts:18, :38, :55, :173).
var ct4Emulated = []string{
	"devin-cli-agentic",
	"duckduckgo-web",
	"cloudflare-playground",
	"uncloseai",
}

func TestCT4Entry403ProvidersAreToSGrey(t *testing.T) {
	for _, id := range ct4Entry403ToSGrey {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("entry-403 provider %q missing from catalog", id)
		}
		if !p.ToSGrey {
			t.Fatalf("entry-403 provider %q is not flagged ToSGrey", id)
		}
		if len(p.BaseURLs) != 0 {
			t.Fatalf("entry-403 provider %q unexpectedly has base URLs (must stay un-addable)", id)
		}
	}
}

func TestCT4ToolCallingMarkers(t *testing.T) {
	for _, id := range ct4Emulated {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("emulated provider %q missing from catalog", id)
		}
		if p.ToolCalling != ToolCallingEmulated {
			t.Fatalf("provider %q: ToolCalling = %q, want %q", id, p.ToolCalling, ToolCallingEmulated)
		}
	}
	// minimax-code is the one no-auth entry the reference marks
	// toolCalling "native" (noauth.ts:130).
	p, ok := ByID("minimax-code")
	if !ok {
		t.Fatal("minimax-code missing from catalog")
	}
	if p.ToolCalling != ToolCallingSupported {
		t.Fatalf("minimax-code: ToolCalling = %q, want %q", p.ToolCalling, ToolCallingSupported)
	}
	if p.ToSGrey {
		t.Fatal("minimax-code must NOT be flagged ToSGrey (native tool calling, local profile auth)")
	}
	// aihorde is NOT marked emulated anywhere in the reference; its
	// notice states tool calling is unavailable, so it is unknown —
	// never emulated, never supported.
	p, ok = ByID("aihorde")
	if !ok {
		t.Fatal("aihorde missing from catalog")
	}
	if p.ToolCalling != ToolCallingUnknown {
		t.Fatalf("aihorde: ToolCalling = %q, want %q", p.ToolCalling, ToolCallingUnknown)
	}
}

func TestCT4EmulatedNeverCountsAsToolSupport(t *testing.T) {
	for _, id := range ct4Emulated {
		p, _ := ByID(id)
		if p.SupportsTools() {
			t.Fatalf("emulated provider %q counts as tool-capable", id)
		}
	}
	p, _ := ByID("aihorde")
	if p.SupportsTools() {
		t.Fatal("aihorde (tool calling unavailable) counts as tool-capable")
	}
	// The rule is on the value, not the entry: an emulated or
	// unknown/unset marker never satisfies a tool-capable check,
	// only "supported" does.
	for _, tc := range []ToolCalling{ToolCallingEmulated, ToolCallingUnknown, ""} {
		if (Provider{ToolCalling: tc}).SupportsTools() {
			t.Fatalf("ToolCalling %q counts as tool-capable", tc)
		}
	}
	if !(Provider{ToolCalling: ToolCallingSupported}).SupportsTools() {
		t.Fatal("ToolCallingSupported does not count as tool-capable")
	}
	if p, _ := ByID("minimax-code"); !p.SupportsTools() {
		t.Fatal("minimax-code (native tool calling) does not count as tool-capable")
	}
}

func TestCT4OtherNoAuthProvidersUnchanged(t *testing.T) {
	// Scope guard: only the five entry-403 providers were flagged.
	// The remaining no-auth entries keep ToSGrey false.
	for _, id := range []string{"veoaifree-web", "auggie", "zcode", "minimax-code", "codex-app-server"} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("no-auth provider %q missing from catalog", id)
		}
		if p.ToSGrey {
			t.Fatalf("no-auth provider %q unexpectedly flagged ToSGrey (CT-4 scope is the five entry-403 providers)", id)
		}
	}
}

// B4 (Block 1): the ToS-avoid marker is a separate field from
// ToSGrey. ToSGrey is the catalog fact (hard refusal at add/load/auth
// time); ToSAvoid is the routing-facing preference: avoid this
// provider as a routing target when a safer route exists.
// AvoidAsTarget wires both: a provider is avoided when either the
// dedicated marker or the grey flag is set.
func TestToSAvoidField(t *testing.T) {
	for _, id := range ct4Entry403ToSGrey {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("entry-403 provider %q missing from catalog", id)
		}
		if !p.ToSAvoid {
			t.Errorf("entry-403 provider %q: ToSAvoid = false, want true (ToS-grey providers must be avoided as routing targets)", id)
		}
		if !p.AvoidAsTarget() {
			t.Errorf("entry-403 provider %q: AvoidAsTarget() = false, want true", id)
		}
	}
	// The other five no-auth entries are legitimate local/CLI or
	// video services, not ToS-grey: no avoid marker, no avoid.
	for _, id := range []string{"veoaifree-web", "auggie", "zcode", "minimax-code", "codex-app-server"} {
		p, ok := ByID(id)
		if !ok {
			t.Fatalf("no-auth provider %q missing from catalog", id)
		}
		if p.ToSAvoid {
			t.Errorf("no-auth provider %q: ToSAvoid = true, want false", id)
		}
		if p.AvoidAsTarget() {
			t.Errorf("no-auth provider %q: AvoidAsTarget() = true, want false", id)
		}
	}
	// The rule is on the value, not the entry: the separate field
	// carries the signal on its own — a provider flagged ToSAvoid
	// without ToSGrey is still avoided, and a provider with
	// neither flag is not.
	if !(Provider{ToSAvoid: true}).AvoidAsTarget() {
		t.Error("Provider{ToSAvoid: true}.AvoidAsTarget() = false, want true")
	}
	if (Provider{ToSGrey: true}).AvoidAsTarget() != true {
		t.Error("Provider{ToSGrey: true}.AvoidAsTarget() = false, want true")
	}
	if (Provider{}).AvoidAsTarget() {
		t.Error("Provider{}.AvoidAsTarget() = true, want false")
	}
}

// B4 (Block 1): ToSAvoid is a safety flag — mergeVerified must never
// drop it, from either side (same OR rule as ToSGrey).
func TestToSAvoidSurvivesMerge(t *testing.T) {
	if got := mergeVerified(Provider{ID: "x", ToSAvoid: true}, Provider{ID: "x", Verification: VerificationDocs}); !got.ToSAvoid {
		t.Error("mergeVerified dropped ToSAvoid from the reference entry")
	}
	if got := mergeVerified(Provider{ID: "x"}, Provider{ID: "x", Verification: VerificationDocs, ToSAvoid: true}); !got.ToSAvoid {
		t.Error("mergeVerified did not pick up ToSAvoid from the verified entry")
	}
}

// B4 (Block 1): every CategoryNoAuth entry must carry an explicit
// ToolCalling value. "" (unset) is not a value: the reference states
// nothing for veoaifree-web, auggie, zcode and codex-app-server, and
// that silence is recorded as ToolCallingUnknown — the same as
// aihorde, whose notice states tool calling is unavailable
// (noauth.ts). The count (10) is the A5-measured set.
func TestNoAuthToolCallingCoverage(t *testing.T) {
	var ids []string
	for _, p := range All() {
		if p.Category != CategoryNoAuth {
			continue
		}
		ids = append(ids, p.ID)
		if p.ToolCalling == "" {
			t.Errorf("no-auth provider %q: ToolCalling unset (want an explicit value: supported, emulated or unknown)", p.ID)
		}
	}
	if len(ids) != 10 {
		t.Fatalf("no-auth set = %d providers (%v), want 10 (A5-measured set)", len(ids), ids)
	}
}
