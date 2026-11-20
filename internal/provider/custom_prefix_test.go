package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAddContextCustomPrefixConvention asserts that custom provider names
// carrying standard prefixes (openai-compatible-, anthropic-compatible-,
// anthropic-compatible-cc-) have their wire format automatically prescribed.
func TestAddContextCustomPrefixConvention(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"test-model"}]}`))
	}))
	defer srv.Close()

	ctx := context.Background()
	reg, _ := newCatalogTestRegistry(t)

	// 1. anthropic-compatible-
	p1, err := reg.AddContext(ctx, "anthropic-compatible-endpoint", srv.URL+"/anthropic", "key-1")
	if err != nil {
		t.Fatalf("AddContext anthropic-compatible: %v", err)
	}
	if p1.WireFormat != "anthropic" {
		t.Fatalf("p1.WireFormat = %q, want %q", p1.WireFormat, "anthropic")
	}

	// 2. anthropic-compatible-cc-
	p2, err := reg.AddContext(ctx, "anthropic-compatible-cc-endpoint", srv.URL+"/cc", "key-2")
	if err != nil {
		t.Fatalf("AddContext anthropic-compatible-cc: %v", err)
	}
	if p2.WireFormat != "anthropic" {
		t.Fatalf("p2.WireFormat = %q, want %q", p2.WireFormat, "anthropic")
	}

	// 3. openai-compatible-
	p3, err := reg.AddContext(ctx, "openai-compatible-endpoint", srv.URL+"/openai", "key-3")
	if err != nil {
		t.Fatalf("AddContext openai-compatible: %v", err)
	}
	if p3.WireFormat != "openai" {
		t.Fatalf("p3.WireFormat = %q, want %q", p3.WireFormat, "openai")
	}
}
