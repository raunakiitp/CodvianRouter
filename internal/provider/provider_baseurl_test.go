package provider

import (
	"errors"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/crypto"
)

// Agent-card nit-2: a provider baseURL is canonicalized at registration
// time (NormalizeBaseURL, via ValidateBaseURL in Registry.addProvider),
// so spellings that differ only by trailing slash or scheme/host case
// register to the same stored value. The path keeps its case.

func TestNormalizeBaseURLTrailingSlash(t *testing.T) {
	clean := "https://api.example.com/v1"
	for _, in := range []string{
		"https://api.example.com/v1/",
		"https://api.example.com/v1///",
		"  https://api.example.com/v1/  ",
	} {
		if got := NormalizeBaseURL(in); got != clean {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", in, got, clean)
		}
	}
	if got := NormalizeBaseURL("https://api.example.com/"); got != "https://api.example.com" {
		t.Errorf("NormalizeBaseURL(%q) = %q, want %q", "https://api.example.com/", got, "https://api.example.com")
	}
	if NormalizeBaseURL("https://api.example.com/v1/") != NormalizeBaseURL(clean) {
		t.Error("trailing-slash input and clean input must normalize to the same value")
	}
}

func TestNormalizeBaseURLSchemeHostCase(t *testing.T) {
	// Mixed-case scheme/host normalizes; path case is left alone.
	if got := NormalizeBaseURL("HTTPS://API.Example.COM/V1/"); got != "https://api.example.com/V1" {
		t.Errorf("NormalizeBaseURL(%q) = %q, want %q", "HTTPS://API.Example.COM/V1/", got, "https://api.example.com/V1")
	}
	if got := NormalizeBaseURL("HTTPS://API.Example.COM/V1"); got != NormalizeBaseURL("https://api.example.com/V1") {
		t.Errorf("mixed-case and clean spellings differ: %q vs %q", got, NormalizeBaseURL("https://api.example.com/V1"))
	}
	// Port survives, host still lowercased.
	if got := NormalizeBaseURL("https://Example.COM:8443/Path/"); got != "https://example.com:8443/Path" {
		t.Errorf("NormalizeBaseURL with port = %q, want %q", got, "https://example.com:8443/Path")
	}
}

func TestNormalizeBaseURLDifferentPathNotCollapsed(t *testing.T) {
	a := NormalizeBaseURL("https://api.example.com/v1")
	b := NormalizeBaseURL("https://api.example.com/v2")
	if a == b {
		t.Errorf("different paths collapsed: %q == %q", a, b)
	}
	// Path case alone is a real difference: /V1 is not /v1.
	upper := NormalizeBaseURL("https://api.example.com/V1")
	lower := NormalizeBaseURL("https://api.example.com/v1")
	if upper == lower {
		t.Errorf("path case collapsed: %q == %q", upper, lower)
	}
}

func TestValidateBaseURLMixedCase(t *testing.T) {
	got, err := ValidateBaseURL("HTTPS://API.Example.COM/v1/", false)
	if err != nil {
		t.Fatalf("ValidateBaseURL mixed case: %v", err)
	}
	if got != "https://api.example.com/v1" {
		t.Errorf("ValidateBaseURL = %q, want %q", got, "https://api.example.com/v1")
	}
}

func TestRegistryAddNormalizesBaseURL(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)
	reg, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(cipher), WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	// Trailing slash at registration stores the clean URL.
	p, err := reg.Add(srv.URL+"/", "sk-test")
	if err != nil {
		t.Fatalf("Add with trailing slash: %v", err)
	}
	if p.BaseURL != srv.URL {
		t.Errorf("stored BaseURL = %q, want %q", p.BaseURL, srv.URL)
	}
	// The clean spelling is now a duplicate: same canonical value.
	if _, err := reg.Add(srv.URL, "sk-test"); !errors.Is(err, ErrProviderExists) {
		t.Errorf("re-adding clean spelling: err = %v, want ErrProviderExists", err)
	}
	if _, err := reg.Add(srv.URL+"/", "sk-test"); !errors.Is(err, ErrProviderExists) {
		t.Errorf("re-adding trailing-slash spelling: err = %v, want ErrProviderExists", err)
	}
	// GetByBaseURL finds it under either spelling.
	if _, ok := reg.GetByBaseURL(srv.URL + "/"); !ok {
		t.Error("GetByBaseURL with trailing slash did not find the provider")
	}
}

func TestRegistryAddNormalizesSchemeHostCase(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse mock URL: %v", err)
	}
	// Same listener via the localhost hostname, upper-case scheme/host
	// and a trailing slash: must store the canonical lower-case form.
	raw := "HTTP://LOCALHOST:" + u.Port() + "/"
	want := "http://localhost:" + u.Port()

	dir := t.TempDir()
	vault, err := crypto.NewFromSecret([]byte("test-master-secret-for-registry!"), dir)
	if err != nil {
		t.Fatalf("test vault: %v", err)
	}
	cipher := NewVaultCipher(vault)
	reg, err := NewFileRegistry(filepath.Join(dir, "providers.json"), WithCipher(cipher), WithRegistryAllowPrivateDestinations(true))
	if err != nil {
		t.Fatalf("NewFileRegistry: %v", err)
	}
	p, err := reg.Add(raw, "sk-test")
	if err != nil {
		t.Fatalf("Add(%q): %v", raw, err)
	}
	if p.BaseURL != want {
		t.Errorf("stored BaseURL = %q, want %q", p.BaseURL, want)
	}
}
