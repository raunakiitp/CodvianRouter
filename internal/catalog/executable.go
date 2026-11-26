package catalog

// executable.go — the catalogue-side statement of which wire formats can
// serve the OpenAI-compatible passthrough endpoints (/v1/embeddings,
// /v1/rerank, /v1/images/*, /v1/audio/*).
//
// This exists because the executable matrix lives in package catalog and
// cannot import internal/provider (provider imports catalog). It is a
// deliberate mirror of provider.WireFormatServesPassthrough, and
// TestPassthroughRuleMatchesRuntime (internal/capability) fails if the two
// ever disagree — so the matrix cannot drift from the router's gates
// again (P2-PROV-001: the matrix claimed gemini/vertex/cohere served
// embeddings, which the runtime refuses).

// EndpointPassthroughSupported reports whether a provider on this wire
// format can serve the passthrough endpoints. Mirror of
// provider.WireFormatServesPassthrough.
func EndpointPassthroughSupported(wire string) bool {
	return wire == "" || wire == string(WireOpenAI)
}
