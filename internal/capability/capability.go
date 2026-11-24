package capability

// Unified provider/model capability registry (P1.5).
//
// What this package is: a single, evidence-backed source of truth for
// every capability a model may or may not have. Each entry carries
// explicit provenance so a consumer can distinguish "the provider's
// docs say this" from "a live probe saw it" from "an operator set it".
//
// What it is NOT: an inference engine. Unknown means unknown — it is
// never promoted to supported, never demoted to unsupported, and it
// never satisfies a "must be verified capable" filter.

import (
	"time"
)

// Capability names the capability being recorded.
type Capability string

const (
	CapToolCalling      Capability = "tool_calling"
	CapVision           Capability = "vision"
	CapImageInput       Capability = "image_input"
	CapImageOutput      Capability = "image_output"
	CapAudioInput       Capability = "audio_input"
	CapAudioOutput      Capability = "audio_output"
	CapVideo            Capability = "video"
	CapReasoning        Capability = "reasoning"
	CapStructuredOutput Capability = "structured_output"
	CapJSONMode         Capability = "json_mode"
	CapStreaming        Capability = "streaming"
	CapContextWindow    Capability = "context_window"
	CapMaxOutputTokens  Capability = "max_output_tokens"
	CapAPIChat          Capability = "api_chat"
	CapAPIResponses     Capability = "api_responses"
	CapAPIEmbeddings    Capability = "api_embeddings"
	CapAPIRerank        Capability = "api_rerank"
	CapAPIImages        Capability = "api_images"
	CapAPIAudio         Capability = "api_audio"
)

// Support is the recorded support state. Unknown means "no affirmative
// evidence found" — it is neither supported nor unsupported, and MUST
// NOT satisfy a "verified capable" filter.
type Support string

const (
	SupportSupported   Support = "supported"
	SupportUnsupported Support = "unsupported"
	SupportUnknown     Support = "unknown"
)

// ProvenanceSource names the origin of the evidence.
type ProvenanceSource string

const (
	SourceCatalogDocs       ProvenanceSource = "catalog_docs"        // provider's own published docs
	SourceCatalogReference  ProvenanceSource = "catalog_reference"   // reference router's metadata
	SourceModelsDevSync     ProvenanceSource = "models_dev_sync"     // opt-in models.dev sync
	SourceLiveProbe         ProvenanceSource = "live_probe"          // /v1/models or capability probe
	SourceOperatorConfig    ProvenanceSource = "operator_config"     // operator-set in stored provider
	SourceCatalogWireFormat ProvenanceSource = "catalog_wire_format" // provider WireFormats implies streaming
	SourceUnknown           ProvenanceSource = "unknown"
)

// VerificationLevel is the strength of the verification.
type VerificationLevel string

const (
	VerificationVerified   VerificationLevel = "verified"   // affirmative report from a trusted source
	VerificationUnverified VerificationLevel = "unverified" // operator-set or unconfirmed
)

// Provenance records where a capability claim came from and how
// strongly it is backed.
type Provenance struct {
	Source       ProvenanceSource  `json:"source"`
	Verification VerificationLevel `json:"verification"`
	ObservedAt   time.Time         `json:"observed_at,omitempty"` // RFC3339 when non-zero
	Confidence   string            `json:"confidence,omitempty"`  // "high" / "medium" / "low"
	Note         string            `json:"note,omitempty"`        // human-readable detail
}

// Entry is one (capability, support, provenance) triple.
type Entry struct {
	Capability Capability `json:"capability"`
	Support    Support    `json:"support"`
	Provenance Provenance `json:"provenance"`
}

// Verified returns true iff support is Supported AND provenance
// verification is Verified. Unknown or Unverified never verifies.
func (e Entry) Verified() bool {
	return e.Support == SupportSupported && e.Provenance.Verification == VerificationVerified
}

// HasSupport reports whether the entry has at least the given
// support level (Supported > Unknown > Unsupported).
func (e Entry) HasSupport(min Support) bool {
	rank := map[Support]int{
		SupportUnsupported: 0,
		SupportUnknown:     1,
		SupportSupported:   2,
	}
	return rank[e.Support] >= rank[min]
}

// ModelCapabilities maps capabilities to their entries for one
// (provider, model). It is the unit the router filters on.
type ModelCapabilities map[Capability]Entry

// HasVerified is a convenience: true iff the model has a Verified
// entry for the capability.
func (mc ModelCapabilities) HasVerified(c Capability) bool {
	if e, ok := mc[c]; ok {
		return e.Verified()
	}
	return false
}

// HasSupportAtLeast is a convenience: true iff the model has an entry
// with support >= min (SupportUnknown counts as > Unsupported).
func (mc ModelCapabilities) HasSupportAtLeast(c Capability, min Support) bool {
	if e, ok := mc[c]; ok {
		return e.HasSupport(min)
	}
	return false
}

// ProvenanceSummary returns a concise string for logging/dashboards.
func (p Provenance) ProvenanceSummary() string {
	if p.ObservedAt.IsZero() {
		return string(p.Source)
	}
	return string(p.Source) + " @ " + p.ObservedAt.Format(time.RFC3339)
}
