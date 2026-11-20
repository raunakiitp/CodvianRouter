package wire

import (
	"net/http"
	"testing"
)

func TestRetryableStatus(t *testing.T) {
	// Statuses that are a property of the target: another target
	// may serve the same request.
	retryable := []int{402, 404, 408, 429, 500, 502, 503, 504, 529}
	for _, status := range retryable {
		if !RetryableStatus(status) {
			t.Errorf("RetryableStatus(%d) = false, want true", status)
		}
	}
	// Statuses that are a property of the request: they fail
	// identically on every target and must stop the chain.
	notRetryable := []int{200, 400, 401, 403, 405, 406, 409, 410, 422, 501}
	for _, status := range notRetryable {
		if RetryableStatus(status) {
			t.Errorf("RetryableStatus(%d) = true, want false", status)
		}
	}
}

func TestRetryableStatusForEndpoint(t *testing.T) {
	// 405/501 are retryable only on /v1/responses and
	// /v1/embeddings: the target lacks that endpoint.
	for _, ep := range []string{"responses", "embeddings", "/v1/responses", "/v1/embeddings"} {
		for _, status := range []int{405, 501} {
			if !RetryableStatusForEndpoint(status, ep) {
				t.Errorf("RetryableStatusForEndpoint(%d, %q) = false, want true", status, ep)
			}
		}
	}
	for _, ep := range []string{"", "chat/completions", "/v1/chat/completions", "models"} {
		for _, status := range []int{405, 501} {
			if RetryableStatusForEndpoint(status, ep) {
				t.Errorf("RetryableStatusForEndpoint(%d, %q) = true, want false", status, ep)
			}
		}
	}
	// The endpoint rule changes nothing for other statuses.
	if !RetryableStatusForEndpoint(529, "responses") || !RetryableStatusForEndpoint(404, "") {
		t.Errorf("endpoint rule must not change non-405/501 statuses")
	}
	if RetryableStatusForEndpoint(400, "responses") {
		t.Errorf("400 must stay non-retryable even on /v1/responses")
	}
}

func TestWireMappersRetryable402404529(t *testing.T) {
	// The shared taxonomy must show up in every wire mapper: 402
	// (that target's billing), 404 (that target lacks the model)
	// and 529 (overload) are all retryable; 400/401/403 stay put.
	h := http.Header{}
	cases := []struct {
		name string
		m    func(int, http.Header, []byte) error
	}{
		{"anthropic", MapError},
		{"gemini", MapGeminiError},
		{"cohere", MapCohereError},
		{"bedrock", MapBedrockError},
	}
	for _, tc := range cases {
		for _, status := range []int{402, 404, 529} {
			err := tc.m(status, h, []byte(`{"error":{"message":"boom"}}`))
			if !isWireRetryable(err) {
				t.Errorf("%s MapError(%d).Retryable = false, want true", tc.name, status)
			}
		}
		for _, status := range []int{400, 401, 403, 422} {
			err := tc.m(status, h, []byte(`{"error":{"message":"boom"}}`))
			if isWireRetryable(err) {
				t.Errorf("%s MapError(%d).Retryable = true, want false", tc.name, status)
			}
		}
	}
}

// isWireRetryable reads the Retryable flag off any of the four wire
// error types without importing provider (which would be a cycle).
func isWireRetryable(err error) bool {
	switch e := err.(type) {
	case *AnthropicError:
		return e.Retryable
	case *GeminiError:
		return e.Retryable
	case *CohereError:
		return e.Retryable
	case *BedrockError:
		return e.Retryable
	}
	return false
}

func TestWireMappersModelNotFound400(t *testing.T) {
	// Entry 151: a 400 whose body signals model-not-found is
	// retryable on every wire; a 400 without the signal is not.
	// The bodies use each wire's own error shape.
	h := http.Header{}
	dead := map[string][]byte{
		"anthropic": []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"The model ` + "`gpt-9`" + ` does not exist"}}`),
		"gemini":    []byte(`{"error":{"message":"models/gemini-x is not found","code":400,"status":"INVALID_ARGUMENT"}}`),
		"cohere":    []byte(`{"message":"model not found"}`),
		"bedrock":   []byte(`{"message":"The provided model identifier is invalid.","__type":"ValidationException"}`),
	}
	malformed := map[string][]byte{
		"anthropic": []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"temperature must be <= 1"}}`),
		"gemini":    []byte(`{"error":{"message":"bad temperature","code":400,"status":"INVALID_ARGUMENT"}}`),
		"cohere":    []byte(`{"message":"temperature must be <= 1"}`),
		"bedrock":   []byte(`{"message":"1 validation error detected","__type":"ValidationException"}`),
	}
	mappers := map[string]func(int, http.Header, []byte) error{
		"anthropic": MapError,
		"gemini":    MapGeminiError,
		"cohere":    MapCohereError,
		"bedrock":   MapBedrockError,
	}
	for name, m := range mappers {
		if err := m(400, h, dead[name]); !isWireRetryable(err) {
			t.Errorf("%s MapError(400 model-not-found).Retryable = false, want true", name)
		}
		if err := m(400, h, malformed[name]); isWireRetryable(err) {
			t.Errorf("%s MapError(400 malformed).Retryable = true, want false", name)
		}
	}
}

func TestIsModelNotFoundSignal(t *testing.T) {
	yes := []string{
		"The model `gpt-9` does not exist",
		"model: claude-x not found",
		"models/gemini-x is not found",
		"The provided model identifier is invalid.",
		"not_found_error model: x",
		"MODEL_NOT_FOUND",
	}
	for _, text := range yes {
		if !IsModelNotFoundSignal(text) {
			t.Errorf("IsModelNotFoundSignal(%q) = false, want true", text)
		}
	}
	no := []string{
		"temperature must be <= 1",
		"unknown model parameter \"foo\" is not supported",
		"content filtered by policy",
		"",
	}
	for _, text := range no {
		if IsModelNotFoundSignal(text) {
			t.Errorf("IsModelNotFoundSignal(%q) = true, want false", text)
		}
	}
}

func TestIsContentRefusalSignal(t *testing.T) {
	yes := []string{
		"content_filter",
		"content filtered due to policy",
		"This request violates our content policy",
		"blocked by safety filters",
		"guardrail_intervened",
		"disallowed content",
		"harmful content detected",
		"content moderation refusal",
		"prohibited content",
	}
	for _, text := range yes {
		if !IsContentRefusalSignal(text) {
			t.Errorf("IsContentRefusalSignal(%q) = false, want true", text)
		}
	}
	no := []string{
		"temperature must be <= 1",
		"The model `gpt-9` does not exist",
		"connection refused",
		"",
		"safety is our top priority",
		"rate limit exceeded",
	}
	for _, text := range no {
		if IsContentRefusalSignal(text) {
			t.Errorf("IsContentRefusalSignal(%q) = true, want false", text)
		}
	}
}
