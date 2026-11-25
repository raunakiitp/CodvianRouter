// P13: Capability registry tri-state honesty.
//
// The capability registry has three support states (Supported /
// Unsupported / Unknown) and two verification levels. The contract
// this file pins down:
//
//   - Unknown NEVER satisfies a "verified" filter. An unknown
//     capability is not a negative answer, so it must not be
//     treated as either "supported" or "verified".
//   - Unsupported NEVER satisfies a "verified" filter either, even
//     when the provenance says the source was verified — an
//     affirmative "no" is not a "yes".
//   - Only Supported + Verified satisfies it.
//   - The support hierarchy is Supported > Unknown > Unsupported:
//     Unknown outranks Unsupported (we do not know yet, which is
//     not the same as "no"), but Unknown never outranks Supported.
package capability

import (
	"testing"
	"time"
)

// TestCapabilityTriStateHonesty asserts the tri-state contract at
// the Entry level, which is what the registry stores and the router
// filters on.
func TestCapabilityTriStateHonesty(t *testing.T) {
	verified := Provenance{
		Source:       SourceCatalogWireFormat,
		Verification: VerificationVerified,
		ObservedAt:   time.Now(),
	}
	unverified := Provenance{
		Source:       SourceOperatorConfig,
		Verification: VerificationUnverified,
		ObservedAt:   time.Now(),
	}

	cases := []struct {
		name         string
		support      Support
		verification VerificationLevel
		wantVerified bool
	}{
		{"supported+verified", SupportSupported, VerificationVerified, true},
		{"supported+unverified", SupportSupported, VerificationUnverified, false},
		{"unknown+verified", SupportUnknown, VerificationVerified, false},
		{"unknown+unverified", SupportUnknown, VerificationUnverified, false},
		{"unsupported+verified", SupportUnsupported, VerificationVerified, false},
		{"unsupported+unverified", SupportUnsupported, VerificationUnverified, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := Entry{
				Capability: CapStreaming,
				Support:    tc.support,
				Provenance: Provenance{Verification: tc.verification},
			}
			if got := e.Verified(); got != tc.wantVerified {
				t.Errorf("Entry.Verified() = %v, want %v", got, tc.wantVerified)
			}
		})
	}

	// The same contract seen through ModelCapabilities, which is the
	// unit the router actually queries.
	mc := ModelCapabilities{
		CapStreaming: Entry{
			Capability: CapStreaming,
			Support:    SupportSupported,
			Provenance: verified,
		},
	}
	if !mc.HasVerified(CapStreaming) {
		t.Error("HasVerified = false; Supported + Verified must satisfy the filter")
	}

	mcUnknown := ModelCapabilities{
		CapStreaming: Entry{
			Capability: CapStreaming,
			Support:    SupportUnknown,
			Provenance: verified,
		},
	}
	if mcUnknown.HasVerified(CapStreaming) {
		t.Error("HasVerified = true for Unknown; unknown must never satisfy the verified filter")
	}

	mcUnsupported := ModelCapabilities{
		CapStreaming: Entry{
			Capability: CapStreaming,
			Support:    SupportUnsupported,
			Provenance: verified,
		},
	}
	if mcUnsupported.HasVerified(CapStreaming) {
		t.Error("HasVerified = true for Unsupported; unsupported must never satisfy the verified filter")
	}

	// An absent capability is not verified either.
	if (ModelCapabilities{}).HasVerified(CapStreaming) {
		t.Error("HasVerified = true for a missing capability; absent must not verify")
	}

	// The unverified provenance path: Supported but only operator-set
	// is still not a verified claim.
	mcUnverified := ModelCapabilities{
		CapStreaming: Entry{
			Capability: CapStreaming,
			Support:    SupportSupported,
			Provenance: unverified,
		},
	}
	if mcUnverified.HasVerified(CapStreaming) {
		t.Error("HasVerified = true for unverified provenance; operator-set is not verified")
	}
}

// TestEntryHasSupportAtLeast verifies the support hierarchy:
// Supported > Unknown > Unsupported.
func TestEntryHasSupportAtLeast(t *testing.T) {
	e := Entry{Support: SupportSupported}
	if !e.HasSupport(SupportSupported) || !e.HasSupport(SupportUnknown) || !e.HasSupport(SupportUnsupported) {
		t.Error("SupportSupported should have all levels")
	}

	e = Entry{Support: SupportUnknown}
	if e.HasSupport(SupportSupported) || !e.HasSupport(SupportUnknown) || !e.HasSupport(SupportUnsupported) {
		t.Error("SupportUnknown should have Unknown and Unsupported only")
	}

	e = Entry{Support: SupportUnsupported}
	if e.HasSupport(SupportSupported) || e.HasSupport(SupportUnknown) || !e.HasSupport(SupportUnsupported) {
		t.Error("SupportUnsupported should have only Unsupported")
	}
}

// TestModelCapabilitiesHasSupportAtLeast mirrors the hierarchy at
// the ModelCapabilities level and covers the missing-entry case.
func TestModelCapabilitiesHasSupportAtLeast(t *testing.T) {
	mc := ModelCapabilities{
		CapStreaming: Entry{Capability: CapStreaming, Support: SupportUnknown},
	}
	if mc.HasSupportAtLeast(CapStreaming, SupportSupported) {
		t.Error("Unknown must not satisfy a Supported floor")
	}
	if !mc.HasSupportAtLeast(CapStreaming, SupportUnknown) {
		t.Error("Unknown must satisfy an Unknown floor")
	}
	if !mc.HasSupportAtLeast(CapStreaming, SupportUnsupported) {
		t.Error("Unknown must satisfy an Unsupported floor")
	}
	if (ModelCapabilities{}).HasSupportAtLeast(CapStreaming, SupportUnsupported) {
		t.Error("a missing capability must not satisfy any floor")
	}
}
