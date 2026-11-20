package catalog

import "testing"

// TestParseCustomPrefix — parity tracker:508, PARTIAL row: no
// openai-compatible-* prefix convention existed. The three
// prefixes now parse to their wire format.
func TestParseCustomPrefix(t *testing.T) {
	for id, want := range map[string]struct {
		prefix CustomPrefix
		rest   string
		wire   WireFormat
	}{
		"openai-compatible-foo":         {CustomPrefixOpenAICompatible, "foo", WireOpenAI},
		"anthropic-compatible-bar":      {CustomPrefixAnthropicCompatible, "bar", WireAnthropic},
		"anthropic-compatible-cc-baz":   {CustomPrefixAnthropicCompatibleCC, "baz", WireAnthropic},
		"openai-compatible-a-b-c":       {CustomPrefixOpenAICompatible, "a-b-c", WireOpenAI},
		"anthropic-compatible-cc-x-y-z": {CustomPrefixAnthropicCompatibleCC, "x-y-z", WireAnthropic},
	} {
		prefix, rest, wire, ok := ParseCustomPrefix(id)
		if !ok || prefix != want.prefix || rest != want.rest || wire != want.wire {
			t.Fatalf("ParseCustomPrefix(%q) = %q %q %q %v, want %q %q %q true",
				id, prefix, rest, wire, ok, want.prefix, want.rest, want.wire)
		}
	}
	// The cc variant must win over the plain Anthropic prefix:
	// "anthropic-compatible-cc-baz" is NOT "anthropic-compatible-"
	// + "cc-baz".
	if prefix, _, _, _ := ParseCustomPrefix("anthropic-compatible-cc-baz"); prefix != CustomPrefixAnthropicCompatibleCC {
		t.Fatalf("cc prefix parsed as %q", prefix)
	}
	for _, id := range []string{
		"", "openai-compatible-", "anthropic-compatible-", "anthropic-compatible-cc-",
		"custom-foo", "openai-foo", "OPENAI-COMPATIBLE-foo",
	} {
		if _, _, _, ok := ParseCustomPrefix(id); ok {
			t.Fatalf("ParseCustomPrefix(%q) matched, want no match", id)
		}
	}
}

func TestCustomProviderApplyPrefix(t *testing.T) {
	c := CustomProvider{Name: "Foo", BaseURL: "https://api.example.com/v1", AuthKind: AuthBearer, WireFormat: WireAnthropic}
	if !c.ApplyPrefix("openai-compatible-foo") {
		t.Fatal("ApplyPrefix did not match openai-compatible-foo")
	}
	if c.WireFormat != WireOpenAI {
		t.Fatalf("wire = %q after openai-compatible prefix, want openai", c.WireFormat)
	}
	// The prefix wins over a previously set wire format.
	c.WireFormat = WireGemini
	if !c.ApplyPrefix("anthropic-compatible-cc-foo") || c.WireFormat != WireAnthropic {
		t.Fatalf("cc prefix did not override wire format (wire = %q)", c.WireFormat)
	}
	// Unprefixed IDs leave the provider untouched.
	c.WireFormat = WireCohere
	if c.ApplyPrefix("plain-name") {
		t.Fatal("ApplyPrefix matched an unprefixed ID")
	}
	if c.WireFormat != WireCohere {
		t.Fatalf("unprefixed ApplyPrefix changed wire to %q", c.WireFormat)
	}
}
