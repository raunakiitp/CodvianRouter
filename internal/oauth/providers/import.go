package providers

// Paste-token import (catalog "antigravity", "agy").
//
// Google Antigravity (and its CLI, catalog "agy") authenticate through
// the operator's own Google account inside the product's tooling; that
// tooling exports its stored credential. The router supports IMPORTING
// that credential blob — it does not perform a Google account login
// itself. Import is allowlisted to exactly these two catalog IDs, and
// the blob's embedded provider field must match the route provider so
// one provider's credential can never be stored under another's route.
//
// Expected blob shape (JSON):
//
//	{
//	  "provider":      "antigravity" | "agy",   (must match the route)
//	  "access_token":  "…",                     (required)
//	  "refresh_token": "…",                     (optional)
//	  "token_type":    "…",                     (optional)
//	  "expires_at":    "2006-01-02T15:04:05Z",  (optional, RFC 3339)
//	  "scope":         "…"                      (optional)
//	}

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PasteImportAllowlist is the complete set of catalog provider IDs the
// paste-token import accepts. Anything else is refused — the import
// path is not a generic "store any token" door.
var PasteImportAllowlist = map[string]bool{
	"antigravity": true,
	"agy":         true,
}

// ImportedCredential is the decoded credential blob.
type ImportedCredential struct {
	Provider     string    `json:"provider"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	Scope        string    `json:"scope"`
}

// ErrImportRefused is returned for any blob the allowlist or the
// provider-match rule refuses.
var ErrImportRefused = fmt.Errorf("providers: paste-token import refused")

// ImportPasteToken decodes and validates a pasted credential blob for
// the given ROUTE provider. Refusals:
//
//   - the route provider is not on the allowlist (antigravity, agy);
//   - the blob's provider field does not match the route provider;
//   - the blob carries no access token;
//   - the blob is not valid JSON.
func ImportPasteToken(routeProvider string, raw []byte) (*ImportedCredential, error) {
	if !PasteImportAllowlist[routeProvider] {
		return nil, fmt.Errorf("%w: %q is not on the paste-import allowlist (antigravity, agy only)", ErrImportRefused, routeProvider)
	}
	var cred ImportedCredential
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cred); err != nil {
		return nil, fmt.Errorf("%w: invalid credential blob: %v", ErrImportRefused, err)
	}
	if strings.TrimSpace(cred.AccessToken) == "" {
		return nil, fmt.Errorf("%w: blob carries no access_token", ErrImportRefused)
	}
	if strings.TrimSpace(cred.Provider) == "" {
		return nil, fmt.Errorf("%w: blob does not name its provider — set \"provider\":%q", ErrImportRefused, routeProvider)
	}
	if cred.Provider != routeProvider {
		return nil, fmt.Errorf("%w: blob provider %q does not match route provider %q", ErrImportRefused, cred.Provider, routeProvider)
	}
	return &cred, nil
}
