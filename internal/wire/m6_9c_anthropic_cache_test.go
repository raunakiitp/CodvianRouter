package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

// M6.9c: the Anthropic content block carries cache_control on the
// wire. The wire layer only CARRIES the field — ConvertRequest never
// invents a marker (the provider client owns the gate and the
// annotation); these tests pin both halves.

func TestM69cBlockCacheControlMarshals(t *testing.T) {
	b, err := json.Marshal(AnthropicBlock{
		Type:         "text",
		Text:         "hello",
		CacheControl: &AnthropicCacheControl{Type: "ephemeral"},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want := `{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}`
	if string(b) != want {
		t.Errorf("block JSON = %s, want %s", b, want)
	}
}

func TestM69cBlockCacheControlWithTTLMarshals(t *testing.T) {
	b, err := json.Marshal(AnthropicBlock{
		Type:         "text",
		Text:         "hello",
		CacheControl: &AnthropicCacheControl{Type: "ephemeral", TTL: "1h"},
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cache_control":{"type":"ephemeral","ttl":"1h"}`) {
		t.Errorf("block JSON = %s, want cache_control with ttl 1h", b)
	}
}

func TestM69cBlockWithoutMarkerOmitsField(t *testing.T) {
	b, err := json.Marshal(AnthropicBlock{Type: "text", Text: "hello"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "cache_control") {
		t.Errorf("unmarked block JSON = %s, must omit cache_control", b)
	}
}

func TestM69cBlockCacheControlRoundTrip(t *testing.T) {
	var b AnthropicBlock
	if err := json.Unmarshal([]byte(`{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}`), &b); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if b.CacheControl == nil || b.CacheControl.Type != "ephemeral" {
		t.Errorf("round-trip CacheControl = %+v, want {Type: ephemeral}", b.CacheControl)
	}
}

func TestM69cConvertRequestNeverMarksByItself(t *testing.T) {
	// The wire converter is a pure conversion: even a long,
	// temp-0, tool-free prompt must come out unmarked — the
	// marker is applied by the provider client after conversion,
	// behind its provider/length gate. If ConvertRequest ever
	// starts marking, this test fails and the layering has
	// regressed.
	long := strings.Repeat("The quick brown fox jumps over the lazy dog. ", 100)
	req, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: mustJSON(long)},
		},
	})
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b), "cache_control") {
		t.Errorf("ConvertRequest emitted cache_control on its own: %s", b)
	}
}

func TestM69cConvertRequestPreservesClientMarker(t *testing.T) {
	// G13: a client-supplied cache_control on a user content
	// part survives ConvertRequest verbatim (it used to be
	// dropped: convertContentParts built fresh blocks without
	// it). ConvertRequest still never INVENTS a marker — the
	// unmarked parts stay unmarked.
	parts, _ := json.Marshal([]ContentPart{
		{Type: "text", Text: "first", CacheControl: &AnthropicCacheControl{Type: "ephemeral", TTL: "1h"}},
		{Type: "text", Text: "second"},
	})
	req, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: parts},
		},
	})
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 2 {
		t.Fatalf("blocks = %+v, want 2", req.Messages)
	}
	got := req.Messages[0].Content[0].CacheControl
	if got == nil || got.Type != "ephemeral" || got.TTL != "1h" {
		t.Errorf("first block CacheControl = %+v, want {ephemeral 1h}", got)
	}
	if req.Messages[0].Content[1].CacheControl != nil {
		t.Errorf("second block gained a marker: %+v", req.Messages[0].Content[1])
	}
}

func TestM69cConvertRequestPreservesSystemMarker(t *testing.T) {
	// G13: a client-supplied cache_control on a system part is
	// preserved, and the system prompt then travels in block
	// form (SystemBlocks) — the string form cannot carry a
	// marker. The joined System text stays available for
	// readers that only look at the string.
	sysParts, _ := json.Marshal([]ContentPart{
		{Type: "text", Text: "Be helpful.", CacheControl: &AnthropicCacheControl{Type: "ephemeral"}},
	})
	req, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "system", Content: sysParts},
			{Role: "user", Content: mustJSON("hello")},
		},
	})
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if req.System != "Be helpful." {
		t.Errorf("System = %q, want the joined system text", req.System)
	}
	if len(req.SystemBlocks) != 1 || req.SystemBlocks[0].CacheControl == nil {
		t.Fatalf("SystemBlocks = %+v, want 1 marked block", req.SystemBlocks)
	}
	// And the block form is what gets marshalled.
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"system":[{"type":"text","text":"Be helpful.","cache_control":{"type":"ephemeral"}}]`) {
		t.Errorf("marshalled system is not the marked block form: %s", b)
	}
}

func TestM69cAnthropicRequestSystemRoundTrip(t *testing.T) {
	// System accepts both forms on decode: a string stays a
	// string, and a block array repopulates SystemBlocks (with
	// System joined from the block texts).
	var fromString AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[],"system":"Be brief."}`), &fromString); err != nil {
		t.Fatalf("Unmarshal string system: %v", err)
	}
	if fromString.System != "Be brief." || len(fromString.SystemBlocks) != 0 {
		t.Errorf("string system decoded as System=%q SystemBlocks=%+v", fromString.System, fromString.SystemBlocks)
	}
	var fromBlocks AnthropicRequest
	if err := json.Unmarshal([]byte(`{"model":"m","max_tokens":10,"messages":[],"system":[{"type":"text","text":"Be brief.","cache_control":{"type":"ephemeral"}}]}`), &fromBlocks); err != nil {
		t.Fatalf("Unmarshal block system: %v", err)
	}
	if fromBlocks.System != "Be brief." {
		t.Errorf("block system System = %q, want joined text", fromBlocks.System)
	}
	if len(fromBlocks.SystemBlocks) != 1 || fromBlocks.SystemBlocks[0].CacheControl == nil {
		t.Errorf("block system SystemBlocks = %+v, want 1 marked block", fromBlocks.SystemBlocks)
	}
	// Marshal of the block form round-trips the marker.
	b, err := json.Marshal(fromBlocks)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(b), `"cache_control":{"type":"ephemeral"}`) {
		t.Errorf("marshalled block system lost the marker: %s", b)
	}
}

func TestM69cConvertRequestPreservesImageMarker(t *testing.T) {
	// G13 (B2-35, f790f58): a client-supplied cache_control on an
	// image_url part survives ConvertRequest verbatim on BOTH image
	// branches — data: base64 and https: url — exactly as on text
	// parts. convertImagePart used to take no marker and silently
	// dropped a breakpoint the client asked for. Unmarked image
	// parts stay unmarked; ConvertRequest still never invents one.
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	parts, _ := json.Marshal([]ContentPart{
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "data:image/png;base64," + pngB64}, CacheControl: &AnthropicCacheControl{Type: "ephemeral"}},
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/cat.jpg"}, CacheControl: &AnthropicCacheControl{Type: "ephemeral", TTL: "1h"}},
		{Type: "image_url", ImageURL: &ImageURLPart{URL: "https://example.com/dog.jpg"}},
	})
	req, err := ConvertRequest(ChatCompletionRequest{
		Model: "claude-sonnet-4-5",
		Messages: []ChatMessage{
			{Role: "user", Content: parts},
		},
	})
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if len(req.Messages) != 1 || len(req.Messages[0].Content) != 3 {
		t.Fatalf("blocks = %+v, want 3", req.Messages)
	}
	blocks := req.Messages[0].Content
	if blocks[0].Type != "image" || blocks[0].Source == nil || blocks[0].Source.Type != "base64" {
		t.Errorf("base64 image block = %+v", blocks[0])
	}
	if got := blocks[0].CacheControl; got == nil || got.Type != "ephemeral" || got.TTL != "" {
		t.Errorf("base64 image CacheControl = %+v, want {ephemeral, no TTL}", got)
	}
	if blocks[1].Type != "image" || blocks[1].Source == nil || blocks[1].Source.Type != "url" || blocks[1].Source.URL != "https://example.com/cat.jpg" {
		t.Errorf("url image block = %+v", blocks[1])
	}
	if got := blocks[1].CacheControl; got == nil || got.Type != "ephemeral" || got.TTL != "1h" {
		t.Errorf("url image CacheControl = %+v, want {ephemeral 1h}", got)
	}
	if blocks[2].CacheControl != nil {
		t.Errorf("unmarked image gained a marker: %+v", blocks[2])
	}
}
