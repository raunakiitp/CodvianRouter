package catalog

import (
	"testing"
)

// ExecutableCapability represents the actual executable capabilities
// of a provider client implementation.
type ExecutableCapability struct {
	ProviderID       string
	WireFormat       string
	HasClient        bool
	Chat             bool
	ChatStream       bool
	ChatStreamChunks bool
	Models           bool
	Embeddings       bool
	Images           bool
	Audio            bool
	Tools            bool
	AuthBearer       bool
	AuthXAPIKey      bool
	AuthAPIKey       bool
	AuthXGoogAPIKey  bool
	AuthXFireworks   bool
	AuthOAuth        bool
	AuthExchange     bool
	AuthCustom       bool
	Streaming        bool
	Notes            string
}

// knownWireFormatsWithClients returns the set of wire formats that
// have a corresponding provider.Client implementation in the codebase.
// This is derived from the actual client files in internal/provider/:
// - openai_client.go -> "openai"
// - anthropic_client.go -> "anthropic"
// - gemini_client.go -> "gemini"
// - cohere_client.go -> "cohere"
// - bedrock_client.go -> "bedrock-converse"
// - vertex_client.go -> "vertex"
// - (ollama uses openai client)
var knownWireFormatsWithClients = map[string]bool{
	"openai":           true,
	"anthropic":        true,
	"gemini":           true,
	"cohere":           true,
	"bedrock-converse": true,
	"vertex":           true,
	"ollama":           true, // uses openai client
}

// authKindSupported checks if the auth kind is handled by the provider registry.
func authKindSupported(kind string) bool {
	switch kind {
	case "bearer", "x-api-key", "api-key", "x-goog-api-key",
		"x-fireworks-api-key", "oauth", "exchange", "custom", "none":
		return true
	default:
		return false
	}
}

// toolCallingSupported checks if the provider natively supports tools.
// Based on the catalog's ToolCalling field, or inferred from wire format
// for verified providers that don't have the field explicitly set.
func toolCallingSupported(p Provider) bool {
	if p.ToolCalling == ToolCallingSupported {
		return true
	}
	// For verified providers, infer from wire format
	if p.Verification == VerificationDocs {
		switch p.DefaultWireFormat() {
		case "openai", "anthropic", "gemini", "vertex", "bedrock-converse", "cohere":
			return true
		}
	}
	return false
}

// catalogExecutableMatrix returns the deterministic audit of all
// catalog entries against actual executable transports, authentication,
// endpoint, streaming, tool and model capabilities.
func catalogExecutableMatrix() []ExecutableCapability {
	var matrix []ExecutableCapability
	allProviders := All()

	for _, p := range allProviders {
		if p.Retired {
			continue
		}

		hasClient := false
		primaryWire := ""
		if len(p.WireFormats) > 0 {
			primaryWire = string(p.WireFormats[0])
			hasClient = knownWireFormatsWithClients[primaryWire]
		}

		authBearer, authXAPIKey, authAPIKey := false, false, false
		authXGoogAPIKey, authXFireworks, authOAuth := false, false, false
		authExchange, authCustom := false, false
		for _, ep := range p.Endpoints {
			if authKindSupported(string(ep.AuthKind)) {
				switch ep.AuthKind {
				case AuthBearer:
					authBearer = true
				case AuthXAPIKey:
					authXAPIKey = true
				case AuthAPIKey:
					authAPIKey = true
				case AuthXGoogAPIKey:
					authXGoogAPIKey = true
				case AuthXFireworksAPIKey:
					authXFireworks = true
				case AuthOAuth:
					authOAuth = true
				case AuthExchange:
					authExchange = true
				case AuthCustom:
					authCustom = true
				}
			}
		}

		streaming := hasClient && (primaryWire != "ollama" || false)

		// Passthrough endpoints are served only by an
		// OpenAI-compatible client that actually exists — the same rule
		// the router's selection gates apply
		// (provider.WireFormatServesPassthrough). P2-PROV-001: the
		// matrix used to claim embeddings/images/audio for gemini,
		// vertex and cohere, which the runtime refuses.
		passthrough := hasClient && EndpointPassthroughSupported(primaryWire)
		embeddings, images, audio := passthrough, passthrough, passthrough

		tools := toolCallingSupported(p)

		notes := ""
		if !hasClient && primaryWire != "" {
			notes = "No client implementation for wire format " + primaryWire
		} else if p.ToSGrey || p.ToSAvoid {
			notes = "ToS-restricted (catalogued, not routed)"
		} else if p.Verification != VerificationDocs {
			notes = "Reference/unverified entry"
		} else {
			notes = "Verified executable"
		}

		ec := ExecutableCapability{
			ProviderID:       p.ID,
			WireFormat:       primaryWire,
			HasClient:        hasClient,
			Chat:             hasClient,
			ChatStream:       hasClient,
			ChatStreamChunks: hasClient && primaryWire != "ollama",
			Models:           hasClient,
			Embeddings:       embeddings,
			Images:           images,
			Audio:            audio,
			Tools:            tools,
			AuthBearer:       authBearer,
			AuthXAPIKey:      authXAPIKey,
			AuthAPIKey:       authAPIKey,
			AuthXGoogAPIKey:  authXGoogAPIKey,
			AuthXFireworks:   authXFireworks,
			AuthOAuth:        authOAuth,
			AuthExchange:     authExchange,
			AuthCustom:       authCustom,
			Streaming:        streaming,
			Notes:            notes,
		}
		matrix = append(matrix, ec)
	}
	return matrix
}

func TestCatalogExecutableMatrix(t *testing.T) {
	matrix := catalogExecutableMatrix()
	allProviders := All()

	execMap := make(map[string]ExecutableCapability)
	for _, ec := range matrix {
		execMap[ec.ProviderID] = ec
	}

	var supported, partiallySupported, unsupported, tosRestricted, missingWire, referenceOnly []string

	for _, p := range allProviders {
		if p.Retired {
			continue
		}

		ec, hasExec := execMap[p.ID]
		if !hasExec {
			continue
		}

		if p.ToSGrey || p.ToSAvoid {
			tosRestricted = append(tosRestricted, p.ID)
			continue
		}

		if !ec.HasClient {
			if p.Verification == VerificationDocs {
				// A3 item 7 (2026-11-27): WireUnknown entries are the
				// docs-verified NON-chat fills (sessions APIs,
				// CodeWhisperer protocol, Connect protocol, MCP,
				// image/workflow APIs) — no wire client exists by
				// design, none is invented, and TestCatalogSweep
				// holds each to a written allow-list reason. Only an
				// entry claiming a REAL wire it has no client for
				// fails here.
				if p.DefaultWireFormat() != WireUnknown {
					missingWire = append(missingWire, p.ID+" (wire format "+ec.WireFormat+" has no client)")
				}
			}
			continue
		}

		if p.Verification != VerificationDocs {
			referenceOnly = append(referenceOnly, p.ID)
			continue
		}

		caps := 0
		maxCaps := 6
		if ec.Chat {
			caps++
		}
		if ec.ChatStream {
			caps++
		}
		if ec.ChatStreamChunks {
			caps++
		}
		if ec.Models {
			caps++
		}
		if ec.Tools {
			caps++
		}
		if ec.Streaming {
			caps++
		}

		if caps == maxCaps {
			supported = append(supported, p.ID)
		} else if caps > 0 {
			partiallySupported = append(partiallySupported, p.ID)
		} else {
			unsupported = append(unsupported, p.ID)
		}
	}

	t.Logf("=== CATALOG EXECUTABLE MATRIX ===")
	t.Logf("Total catalog providers (non-retired): %d", len(allProviders))
	t.Logf("Supported (verified, all capabilities): %d - %v", len(supported), supported)
	t.Logf("Partially supported: %d - %v", len(partiallySupported), partiallySupported)
	t.Logf("Unsupported: %d - %v", len(unsupported), unsupported)
	t.Logf("ToS-restricted: %d - %v", len(tosRestricted), tosRestricted)
	t.Logf("Missing wire client: %d - %v", len(missingWire), missingWire)
	t.Logf("Reference/unverified only: %d - %v", len(referenceOnly), referenceOnly)

	if len(unsupported) > 0 {
		t.Errorf("B1 FAIL: %d verified-from-docs providers are unsupported: %v", len(unsupported), unsupported)
	}
	if len(missingWire) > 0 {
		t.Errorf("B1 FAIL: %d providers have wire format with no client: %v", len(missingWire), missingWire)
	}

	t.Logf("\n=== DETAILED MATRIX (verified-from-docs only) ===")
	for _, ec := range matrix {
		p, ok := ByID(ec.ProviderID)
		if !ok || p.Retired || p.Verification != VerificationDocs {
			continue
		}
		t.Logf("%s: wire=%s client=%v chat=%v stream=%v chunks=%v models=%v tools=%v embed=%v images=%v audio=%v streaming=%v auth(bearer=%v xapikey=%v apikey=%v xgoog=%v xfireworks=%v oauth=%v exchange=%v custom=%v) notes=%s",
			ec.ProviderID, ec.WireFormat, ec.HasClient, ec.Chat, ec.ChatStream, ec.ChatStreamChunks, ec.Models, ec.Tools, ec.Embeddings, ec.Images, ec.Audio, ec.Streaming,
			ec.AuthBearer, ec.AuthXAPIKey, ec.AuthAPIKey, ec.AuthXGoogAPIKey, ec.AuthXFireworks, ec.AuthOAuth, ec.AuthExchange, ec.AuthCustom, ec.Notes)
	}
}

func TestCatalogExecutableMatrixCounts(t *testing.T) {
	allProviders := All()
	activeProviders := 0
	verifiedCount := 0
	for _, p := range allProviders {
		if !p.Retired {
			activeProviders++
			if p.Verification == VerificationDocs {
				verifiedCount++
			}
		}
	}
	t.Logf("Active catalog providers: %d", activeProviders)
	t.Logf("Verified-from-docs providers: %d", verifiedCount)

	matrix := catalogExecutableMatrix()
	t.Logf("Executable capability records generated: %d", len(matrix))

	execMap := make(map[string]bool)
	for _, ec := range matrix {
		if ec.HasClient {
			execMap[ec.ProviderID] = true
		}
	}

	for _, p := range allProviders {
		if p.Retired {
			continue
		}
		// A3 item 7 (2026-11-27): docs-verified fills whose provider
		// documents a NON-chat surface (Devin sessions, Qoder Cloud
		// Agents, Amazon Q/Kiro CodeWhisperer protocol, Cursor Connect,
		// GitHub enterprise Copilot, MCP servers, image/workflow APIs)
		// carry WireUnknown honestly — verified facts, no executable
		// chat client, and no invented wire. The gate exempts exactly
		// these: a docs-verified WireUnknown entry is verified but
		// deliberately not chat-dialable, and TestCatalogSweep still
		// holds every unknown-wire routable entry to a written
		// allow-list reason.
		if p.Verification == VerificationDocs && !execMap[p.ID] && !p.ToSGrey && !p.ToSAvoid &&
			p.DefaultWireFormat() != WireUnknown {
			t.Errorf("Verified provider %s has no executable client", p.ID)
		}
	}
}
