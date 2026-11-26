package router

// P2-MONEY-003: Gemini native attribution — EndpointGeminiGenerateContent
// maps to the chat surface for routing but carries its own kind so
// Gemini spend rows are attributable separately from chat completions.

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestP2_MONEY_003_GeminiEndpointProjection verifies that
// EndpointGeminiGenerateContent maps to autorouter.EndpointChat
// (same routing surface as chat) but has its own kind for analytics.
func TestP2_MONEY_003_GeminiEndpointProjection(t *testing.T) {
	ep, err := endpointForProjection(provider.EndpointGeminiGenerateContent)
	if err != nil {
		t.Fatalf("endpointForProjection returned error: %v", err)
	}
	if ep != autorouter.EndpointChat {
		t.Errorf("endpoint = %v, want %v (Gemini maps to chat surface)", ep, autorouter.EndpointChat)
	}
}

// TestP2_MONEY_003_EndpointConstantExists verifies the constant is
// declared and distinct from other chat kinds.
func TestP2_MONEY_003_EndpointConstantExists(t *testing.T) {
	if provider.EndpointGeminiGenerateContent == provider.EndpointChatCompletions {
		t.Error("EndpointGeminiGenerateContent equals EndpointChatCompletions (should be distinct)")
	}
	if provider.EndpointGeminiGenerateContent == provider.EndpointMessages {
		t.Error("EndpointGeminiGenerateContent equals EndpointMessages (should be distinct)")
	}
	if provider.EndpointGeminiGenerateContent == provider.EndpointResponses {
		t.Error("EndpointGeminiGenerateContent equals EndpointResponses (should be distinct)")
	}
	if string(provider.EndpointGeminiGenerateContent) != "gemini_generate_content" {
		t.Errorf("string value = %q, want %q", string(provider.EndpointGeminiGenerateContent), "gemini_generate_content")
	}
}
