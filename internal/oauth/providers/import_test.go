package providers

import (
	"errors"
	"strings"
	"testing"
)

// TestImportPasteTokenAllowlist pins the allowlist: antigravity and
// agy import; EVERY other provider is refused.
func TestImportPasteTokenAllowlist(t *testing.T) {
	okBlob := func(provider string) string {
		return `{"provider":"` + provider + `","access_token":"agg-token-1","refresh_token":"agg-refresh-1"}`
	}
	// Allowed routes accept a matching blob.
	for _, id := range []string{"antigravity", "agy"} {
		cred, err := ImportPasteToken(id, []byte(okBlob(id)))
		if err != nil {
			t.Fatalf("ImportPasteToken(%q): %v", id, err)
		}
		if cred.AccessToken != "agg-token-1" || cred.RefreshToken != "agg-refresh-1" {
			t.Errorf("ImportPasteToken(%q) decoded %+v", id, cred)
		}
	}
	// Every other provider is refused — including implemented OAuth
	// handlers and the other identity providers.
	for _, id := range []string{"cursor", "codex", "claude", "github-copilot", "chatgpt", "kimi-coding", "kiro", "amazon-q", "gitlab-duo", "not-a-provider"} {
		_, err := ImportPasteToken(id, []byte(okBlob(id)))
		if !errors.Is(err, ErrImportRefused) {
			t.Errorf("ImportPasteToken(%q) error = %v, want ErrImportRefused (allowlist)", id, err)
		}
	}
}

// TestImportPasteTokenBlobProviderMustMatchRoute refuses blobs whose
// embedded provider does not match the route provider — one
// provider's credential can never be stored under another's route.
func TestImportPasteTokenBlobProviderMustMatchRoute(t *testing.T) {
	_, err := ImportPasteToken("agy", []byte(`{"provider":"antigravity","access_token":"t"}`))
	if err == nil || !strings.Contains(err.Error(), "does not match route provider") {
		t.Errorf("mismatched blob error = %v, want route/provider mismatch refusal", err)
	}
	_, err = ImportPasteToken("antigravity", []byte(`{"provider":"agy","access_token":"t"}`))
	if !errors.Is(err, ErrImportRefused) {
		t.Errorf("mismatched blob error = %v, want ErrImportRefused", err)
	}
	_, err = ImportPasteToken("agy", []byte(`{"provider":"cursor","access_token":"t"}`))
	if !errors.Is(err, ErrImportRefused) {
		t.Errorf("foreign blob error = %v, want ErrImportRefused", err)
	}
}

// TestImportPasteTokenShapeValidation covers the blob-shape refusals.
func TestImportPasteTokenShapeValidation(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want string
	}{
		{"bad json", `{not json`, "invalid credential blob"},
		{"no access token", `{"provider":"agy"}`, "no access_token"},
		{"blank access token", `{"provider":"agy","access_token":"  "}`, "no access_token"},
		{"unnamed provider", `{"access_token":"t"}`, "does not name its provider"},
		{"unknown field", `{"provider":"agy","access_token":"t","hax":1}`, "invalid credential blob"},
	}
	for _, tc := range cases {
		_, err := ImportPasteToken("agy", []byte(tc.blob))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// TestImportPasteTokenOptionalFields checks a minimal blob decodes.
func TestImportPasteTokenOptionalFields(t *testing.T) {
	cred, err := ImportPasteToken("antigravity", []byte(`{"provider":"antigravity","access_token":"tok"}`))
	if err != nil {
		t.Fatalf("minimal blob: %v", err)
	}
	if cred.AccessToken != "tok" || cred.RefreshToken != "" {
		t.Errorf("minimal blob decoded %+v", cred)
	}
}
