package provider

// endpoints.go — the single rule that decides whether a wire format can
// serve the OpenAI-compatible passthrough endpoints (/v1/embeddings,
// /v1/rerank, /v1/images/*, /v1/audio/*).
//
// P2-PROV-001: the router's selection gates (supportsEmbeddings,
// supportsRerank, supportsImages, supportsAudio), the catalogue's
// executable matrix and the capability registry's api_* capabilities all
// consult THIS function. They used to encode the rule independently, and
// the catalogue's copy said gemini/vertex/cohere could serve embeddings
// while the runtime refused those clients — so the matrix advertised
// capabilities the router would never select a provider for.
//
// Empty wire means "the Client contract is OpenAI-compatible by
// definition". A future native transport (Cohere /v1/embed, Gemini
// embedContent/generateImages, Bedrock image models) changes this
// function, not the call sites.

// WireFormatServesPassthrough reports whether a client speaking wf can
// serve an OpenAI-wire passthrough endpoint.
func WireFormatServesPassthrough(wf string) bool {
	return wf == "" || wf == WireFormatOpenAI
}
