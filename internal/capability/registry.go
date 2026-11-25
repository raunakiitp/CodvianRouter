package capability

// Registry builds the capability map for every (provider, model) from
// the router's existing evidence sources.
//
// Sources consulted (in priority order; later sources only add or
// strengthen, never weaken a Verified entry):
//
//  1. Catalog Provider.ToolCalling + VerificationStatus
//  2. Provider.Model: Vision/VisionKnown, Audio/AudioKnown, Video/VideoKnown,
//     Tools (tri-state), Reasoning, ContextWindow, FixedParams.MaxTokens
//  3. models.dev sync: context window, prices, modalities (Vision/Audio/Video)
//  4. WireFormats + endpoint streaming → streaming capability
//  5. Operator config (stored provider.Model) — provenance = operator_config,
//     verification = unverified (no affirmative external report)
//
// The builder is deterministic: given the same input sources it
// produces byte-identical ModelCapabilities.

import (
	"sort"
	"time"

	"github.com/raunakiitp/codvian-router/internal/catalog"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// Model builds one model's capability set on demand, from the same
// sources Build uses but without materialising the whole matrix. It is
// the lookup a request-path caller wants: fresh (a registry change is
// visible immediately, no snapshot to refresh) and O(models of one
// provider). ok is false when the provider/model pair is unknown to
// every source.
func (b *Builder) Model(providerID, model string) (ModelCapabilities, bool) {
	if providerID == "" || model == "" {
		return nil, false
	}
	// Registry provider first: the routable source of truth.
	if b.Registry != nil {
		if rp, ok := b.Registry.Get(providerID); ok && rp != nil {
			for _, m := range rp.Models {
				if m.ID == model {
					var cp *catalog.Provider
					if p, ok := catalog.ByID(providerID); ok {
						cp = &p
					} else if rp.CatalogID != "" {
						if p, ok := catalog.ByID(rp.CatalogID); ok {
							cp = &p
						}
					}
					out := map[string]ModelCapabilities{}
					b.buildModels(out, providerID, cp, rp, []string{model})
					mc, found := out[providerID+"|"+model]
					return mc, found
				}
			}
		}
		return nil, false
	}
	// Discovery-only mode (no registry): the catalog is the source.
	cp, ok := catalog.ByID(providerID)
	if !ok {
		return nil, false
	}
	for _, m := range cp.Models {
		if m == model {
			out := map[string]ModelCapabilities{}
			b.buildModels(out, providerID, &cp, nil, []string{model})
			mc, found := out[providerID+"|"+model]
			return mc, found
		}
	}
	return nil, false
}

// ParseCapability maps a config string to a Capability. The second
// return is false for any unknown name — callers reject it at config
// load rather than silently ignoring a capability requirement.
func ParseCapability(s string) (Capability, bool) {
	c := Capability(s)
	switch c {
	case CapToolCalling, CapVision, CapImageInput, CapImageOutput,
		CapAudioInput, CapAudioOutput, CapVideo, CapReasoning,
		CapStructuredOutput, CapJSONMode, CapStreaming, CapContextWindow,
		CapMaxOutputTokens, CapAPIChat, CapAPIResponses, CapAPIEmbeddings,
		CapAPIRerank, CapAPIImages, CapAPIAudio:
		return c, true
	}
	return "", false
}

// KnownCapabilities lists every capability name, for validation error
// messages (so the operator sees the full vocabulary).
func KnownCapabilities() []string {
	return []string{
		string(CapToolCalling), string(CapVision), string(CapImageInput),
		string(CapImageOutput), string(CapAudioInput), string(CapAudioOutput),
		string(CapVideo), string(CapReasoning), string(CapStructuredOutput),
		string(CapJSONMode), string(CapStreaming), string(CapContextWindow),
		string(CapMaxOutputTokens), string(CapAPIChat), string(CapAPIResponses),
		string(CapAPIEmbeddings), string(CapAPIRerank), string(CapAPIImages),
		string(CapAPIAudio),
	}
}

// Builder constructs ModelCapabilities from the router's data.
type Builder struct {
	Registry *provider.Registry

	Sync Modelsync // optional; nil means no models.dev sync active
}

// Modelsync is the minimal interface the builder needs from the
// modelsync package (avoids import cycle).
type Modelsync interface {
	SyncedContextWindow(providerID, model string) (int, bool)
	SyncedPriceUSD(providerID, model string) (input, output float64, ok bool)
	SyncedModality(providerID, model string) (vision, audio, video bool, ok bool)
}

// Build returns a map keyed by "providerID|modelID" → ModelCapabilities.
// Deterministic: same inputs → same outputs (no map iteration order
// dependencies).
func (b *Builder) Build() map[string]ModelCapabilities {
	out := map[string]ModelCapabilities{}

	// Registry providers first: these are the routable ones, and the
	// matrix is keyed by the registry's own provider ID — the same ID
	// the router passes to Config.CapabilityLookup.
	covered := map[string]bool{}
	if b.Registry != nil {
		regs := b.Registry.List()
		sort.SliceStable(regs, func(i, j int) bool { return regs[i].ID < regs[j].ID })
		for _, rp := range regs {
			if rp == nil {
				continue
			}
			var cp *catalog.Provider
			if p, ok := catalog.ByID(rp.ID); ok {
				cp = &p
			} else if rp.CatalogID != "" {
				if p, ok := catalog.ByID(rp.CatalogID); ok {
					cp = &p
				}
			}
			if rp.CatalogID != "" {
				covered[rp.CatalogID] = true
			}
			covered[rp.ID] = true
			ids := make([]string, 0, len(rp.Models))
			for _, m := range rp.Models {
				ids = append(ids, m.ID)
			}
			if cp != nil {
				ids = mergeModelIDs(ids, cp.Models)
			}
			b.buildModels(out, rp.ID, cp, rp, ids)
		}
	}

	// Catalog providers with no registry entry: discovery-only rows
	// (no key configured). They are reported so the matrix answers
	// "what would this provider offer", and they are never routable
	// until added.
	providers := catalog.All()
	sort.SliceStable(providers, func(i, j int) bool { return providers[i].ID < providers[j].ID })
	for _, cp := range providers {
		if covered[cp.ID] {
			continue
		}
		cpCopy := cp
		b.buildModels(out, cp.ID, &cpCopy, nil, mergeModelIDs(nil, cp.Models))
	}
	return out
}

// mergeModelIDs unions two model-ID lists in stable sorted order.
func mergeModelIDs(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, id := range list {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// buildModels fills out["providerID|modelID"] for every model ID, using
// the catalog entry (may be nil for operator-added providers) and the
// registry provider (may be nil for discovery-only providers).
func (b *Builder) buildModels(out map[string]ModelCapabilities, providerID string, cp *catalog.Provider, regProvider *provider.Provider, modelIDs []string) {
	// Provider-level ToolCalling provenance from the catalog; an
	// operator-added provider with no catalog entry has none.
	var toolProv Provenance
	support := SupportUnknown
	if cp != nil {
		switch cp.ToolCalling {
		case catalog.ToolCallingSupported:
			support = SupportSupported
			toolProv = Provenance{
				Source:       SourceCatalogDocs,
				Verification: provVer(cp.Verification),
				ObservedAt:   time.Now(), // catalog load time; best effort
				Confidence:   confidenceFromVer(cp.Verification),
			}
		case catalog.ToolCallingEmulated:
			support = SupportUnsupported // emulated is NOT native tool support
			toolProv = Provenance{
				Source:       SourceCatalogReference,
				Verification: VerificationVerified,
				ObservedAt:   time.Now(),
				Confidence:   "high",
				Note:         "prompt-emulated; not native tool calling",
			}
		default: // unknown / unset
			toolProv = Provenance{
				Source:       SourceCatalogReference,
				Verification: VerificationUnverified,
				ObservedAt:   time.Now(),
				Confidence:   "low",
				Note:         "no tool-calling marker in reference metadata",
			}
		}
	} else {
		toolProv = Provenance{
			Source:       SourceOperatorConfig,
			Verification: VerificationUnverified,
			ObservedAt:   time.Now(),
			Confidence:   "low",
			Note:         "operator-added provider: no catalog tool-calling metadata",
		}
	}

	ids := mergeModelIDs(nil, modelIDs)
	for _, model := range ids {
		mc := ModelCapabilities{}
		mc[CapToolCalling] = Entry{Capability: CapToolCalling, Support: support, Provenance: toolProv}

		// If we have registry model details, enrich.
		if regProvider != nil {
			for _, rm := range regProvider.Models {
				if rm.ID == model {
					b.enrichFromRegistryModel(mc, providerID, rm)
					break
				}
			}
		}

		// Models.dev sync enrichment (if active).
		if b.Sync != nil {
			b.enrichFromModelsync(mc, providerID, model)
		}

		// Wire formats → streaming capability. The registry's wire
		// format wins when the provider was added through the
		// registry; the catalog's is the fallback.
		wireFormats := []catalog.WireFormat(nil)
		if cp != nil {
			wireFormats = cp.WireFormats
		}
		if len(wireFormats) > 0 && wireFormats[0] != catalog.WireUnknown {
			streamProv := Provenance{
				Source:       SourceCatalogWireFormat,
				Verification: VerificationVerified,
				ObservedAt:   time.Now(),
				Confidence:   "high",
				Note:         "provider declares " + string(wireFormats[0]) + " wire format",
			}
			mc[CapStreaming] = Entry{Capability: CapStreaming, Support: SupportSupported, Provenance: streamProv}
			wf := wireFormats[0]
			if wf == catalog.WireOpenAI || wf == catalog.WireAnthropic || wf == catalog.WireGemini {
				mc[CapAPIChat] = Entry{Capability: CapAPIChat, Support: SupportSupported, Provenance: streamProv}
				mc[CapAPIResponses] = Entry{Capability: CapAPIResponses, Support: SupportSupported, Provenance: streamProv}
			}
		} else if len(wireFormats) > 0 && wireFormats[0] == catalog.WireUnknown {
			// Wire format is explicitly unknown: record streaming as
			// unknown with proper provenance, never as supported.
			unknownProv := Provenance{
				Source:       SourceCatalogWireFormat,
				Verification: VerificationUnverified,
				ObservedAt:   time.Now(),
				Confidence:   "low",
				Note:         "provider declares unknown wire format",
			}
			mc[CapStreaming] = Entry{Capability: CapStreaming, Support: SupportUnknown, Provenance: unknownProv}
		}

		out[providerID+"|"+model] = mc
	}
}

func provVer(v catalog.VerificationStatus) VerificationLevel {
	if v == catalog.VerificationDocs {
		return VerificationVerified
	}
	return VerificationUnverified
}

func confidenceFromVer(v catalog.VerificationStatus) string {
	switch v {
	case catalog.VerificationDocs:
		return "high"
	case catalog.VerificationReference:
		return "medium"
	default:
		return "low"
	}
}

// enrichFromRegistryModel adds capabilities from provider.Model.
func (b *Builder) enrichFromRegistryModel(mc ModelCapabilities, providerID string, rm provider.Model) {
	// Vision / ImageInput (same as Vision for input).
	if rm.VisionKnown {
		mc[CapVision] = Entry{
			Capability: CapVision,
			Support:    SupportSupported,
			Provenance: Provenance{
				Source:       SourceModelsDevSync,
				Verification: VerificationVerified,
				ObservedAt:   time.Now(),
				Confidence:   "high",
				Note:         "models.dev modalities table",
			},
		}
		mc[CapImageInput] = mc[CapVision]
	} else if rm.Vision {
		mc[CapVision] = Entry{Capability: CapVision, Support: SupportSupported, Provenance: Provenance{Source: SourceOperatorConfig, Verification: VerificationUnverified, Note: "operator-set, no sync report"}}
	}

	// Audio input.
	if rm.AudioKnown {
		mc[CapAudioInput] = Entry{
			Capability: CapAudioInput,
			Support:    SupportSupported,
			Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev modalities table"},
		}
	} else if rm.Audio {
		mc[CapAudioInput] = Entry{Capability: CapAudioInput, Support: SupportSupported, Provenance: Provenance{Source: SourceOperatorConfig, Verification: VerificationUnverified, Note: "operator-set, no sync report"}}
	}

	// Video input.
	if rm.VideoKnown {
		mc[CapVideo] = Entry{
			Capability: CapVideo,
			Support:    SupportSupported,
			Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev modalities table"},
		}
	} else if rm.Video {
		mc[CapVideo] = Entry{Capability: CapVideo, Support: SupportSupported, Provenance: Provenance{Source: SourceOperatorConfig, Verification: VerificationUnverified, Note: "operator-set, no sync report"}}
	}

	// ToolCalling (model-level overrides catalog).
	if rm.Tools != nil {
		support := SupportSupported
		if !*rm.Tools {
			support = SupportUnsupported
		}
		mc[CapToolCalling] = Entry{
			Capability: CapToolCalling,
			Support:    support,
			Provenance: Provenance{
				Source:       SourceLiveProbe,
				Verification: VerificationVerified,
				ObservedAt:   time.Now(),
				Confidence:   "high",
				Note:         "live /v1/models probe or operator-set",
			},
		}
	}

	// Reasoning (no Known flag — treat as operator config if set).
	if rm.Reasoning {
		mc[CapReasoning] = Entry{
			Capability: CapReasoning,
			Support:    SupportSupported,
			Provenance: Provenance{Source: SourceOperatorConfig, Verification: VerificationUnverified, ObservedAt: time.Now(), Note: "operator-set in model entry"},
		}
	}

	// Context window.
	if rm.ContextWindow > 0 {
		mc[CapContextWindow] = Entry{
			Capability: CapContextWindow,
			Support:    SupportSupported,
			Provenance: Provenance{
				Source:       SourceModelsDevSync,
				Verification: VerificationVerified,
				ObservedAt:   time.Now(),
				Confidence:   "high",
				Note:         "models.dev or operator-set",
			},
		}
	}

	// Max output tokens (from FixedParams or model-level).
	if rm.FixedParams != nil && rm.FixedParams.MaxTokens != nil {
		mc[CapMaxOutputTokens] = Entry{
			Capability: CapMaxOutputTokens,
			Support:    SupportSupported,
			Provenance: Provenance{Source: SourceOperatorConfig, Verification: VerificationVerified, ObservedAt: time.Now(), Note: "operator-pinned FixedParams.MaxTokens"},
		}
	}
}

// enrichFromModelsync adds context window & modality from the sync.
func (b *Builder) enrichFromModelsync(mc ModelCapabilities, providerID, model string) {
	if b.Sync == nil {
		return
	}
	if _, ok := b.Sync.SyncedContextWindow(providerID, model); ok {
		mc[CapContextWindow] = Entry{
			Capability: CapContextWindow,
			Support:    SupportSupported,
			Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev sync"},
		}
	}
	if vision, audio, video, ok := b.Sync.SyncedModality(providerID, model); ok {
		if vision {
			mc[CapVision] = Entry{Capability: CapVision, Support: SupportSupported, Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev modalities table"}}
			mc[CapImageInput] = mc[CapVision]
		}
		if audio {
			mc[CapAudioInput] = Entry{Capability: CapAudioInput, Support: SupportSupported, Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev modalities table"}}
		}
		if video {
			mc[CapVideo] = Entry{Capability: CapVideo, Support: SupportSupported, Provenance: Provenance{Source: SourceModelsDevSync, Verification: VerificationVerified, ObservedAt: time.Now(), Confidence: "high", Note: "models.dev modalities table"}}
		}
	}
}
