package catalog

// WS9D — OAuth connectability inventory and catalog truthfulness
// (final closure item 1).
//
// The reconciled findings register claimed "31 CategoryOAuth providers
// advertised but unconnectable". Source inspection proves a different,
// honest shape: the CategoryOAuth entries are METADATA-ONLY STUBS —
// no base URLs, no endpoints, no auth surfaces, WireUnknown — so the
// catalog does NOT advertise them as connectable, and nothing in the
// connection UI can present them as such. The in-router OAuth flows
// that genuinely exist live elsewhere (OpenRouter PKCE in
// internal/providerlogin, Vertex JWT in provider/vertex_client.go,
// Bedrock SigV4 in provider/bedrock_client.go). These tests pin that
// shape so a future edit cannot start advertising connectability
// without adding the actual machinery — the catalog may never claim
// an OAuth flow the router does not perform.

import (
	"strings"
	"testing"
)

// TestCategoryOAuthInventoryPinsTheTruth pins the verified three-way
// classification of all 31 CategoryOAuth entries (dumped from the
// live catalog on 2026-11-26):
//
//   - 1 CONNECTABLE (external-login model): "claude" — docs-verified
//     anthropic-wire surface with one endpoint (messages) whose
//     AuthKind is oauth and whose Exchange documents the real
//     oauth-pkce/subscription-token flow. The router dials with the
//     token the user obtains; it does not perform the login itself
//     (internal/providerlogin has only OpenRouter) — and the notes
//     must say so.
//   - 2 PARTIALLY DOCUMENTED: "github-copilot", "clinepass" — a base
//     URL from reference metadata, but NO endpoints and NOT
//     docs-verified: no auth surface is advertised, so they are not
//     connectable through the catalog.
//   - 28 METADATA STUBS: no base URLs, no endpoints, WireUnknown.
//
// Any drift from this classification is a deliberate connection
// decision that must ship the real flow with it.
func TestCategoryOAuthInventoryPinsTheTruth(t *testing.T) {
	var oauth []Provider
	for _, p := range All() {
		if p.Category == CategoryOAuth {
			oauth = append(oauth, p)
		}
	}
	if len(oauth) != 31 {
		t.Fatalf("CategoryOAuth entries = %d, want 31 (the pinned inventory)", len(oauth))
	}
	byID := map[string]Provider{}
	for _, p := range oauth {
		byID[p.ID] = p
	}

	// 1. claude — connectable via external login.
	cl, ok := byID["claude"]
	if !ok {
		t.Fatal("claude entry missing from CategoryOAuth inventory")
	}
	if len(cl.Endpoints) != 1 || cl.Endpoints[0].Endpoint != "messages" {
		t.Fatalf("claude endpoints = %+v, want exactly one 'messages' surface", cl.Endpoints)
	}
	ep := cl.Endpoints[0]
	if ep.AuthKind != AuthOAuth || ep.Exchange == nil || ep.Exchange.Kind != "oauth-pkce" {
		t.Fatalf("claude auth = %v exchange %+v, want AuthOAuth with an oauth-pkce Exchange", ep.AuthKind, ep.Exchange)
	}
	if cl.Verification != VerificationDocs {
		t.Errorf("claude verification = %q, want docs-verified (the one OAuth surface the router actually dials)", cl.Verification)
	}
	if !strings.Contains(cl.Notes, "does not perform the login itself") {
		t.Errorf("claude notes = %q, want the honest external-login disclosure (the router has no Claude OAuth client)", cl.Notes)
	}

	// 2. partially documented — base URL but no connectable surface.
	for _, id := range []string{"github-copilot", "clinepass"} {
		p, ok := byID[id]
		if !ok {
			t.Fatalf("expected partially-documented entry %q missing", id)
		}
		if len(p.BaseURLs) == 0 {
			t.Errorf("%q lost its base URL — inventory drift", id)
		}
		if len(p.Endpoints) != 0 {
			t.Errorf("%q declares %d endpoints — an advertised auth surface REQUIRES the real exchange implementation before it can ship", id, len(p.Endpoints))
		}
		if p.Verification == VerificationDocs {
			t.Errorf("%q claims docs-verified — only its base URL was probed, never the auth flow", id)
		}
	}

	// 3. everything else — pure metadata stubs.
	stubs := 0
	for _, p := range oauth {
		if p.ID == "claude" || p.ID == "github-copilot" || p.ID == "clinepass" {
			continue
		}
		stubs++
		if len(p.BaseURLs) != 0 || len(p.Endpoints) != 0 {
			t.Errorf("stub %q grew a connection surface (urls=%v endpoints=%d) — advertise only with the real flow", p.ID, p.BaseURLs, len(p.Endpoints))
		}
		for _, w := range p.WireFormats {
			if w != WireUnknown {
				t.Errorf("stub %q declares wire %q — a concrete wire implies a dialable surface", p.ID, w)
			}
		}
		if p.Verification == VerificationDocs {
			t.Errorf("stub %q claims docs-verified — the router never exercised its flow", p.ID)
		}
	}
	if stubs != 28 {
		t.Fatalf("metadata stubs = %d, want 28 (31 - claude - github-copilot - clinepass); the classification drifted", stubs)
	}

	// The one in-router provider OAuth flow stays OpenRouter (a
	// CategoryAPIKey entry — its PKCE login lives in
	// internal/providerlogin, not the catalog category).
	for _, p := range All() {
		if p.ID == "openrouter" && p.Category != CategoryAPIKey {
			t.Errorf("openrouter category = %q, want api-key", p.Category)
		}
	}
}

// TestExchangeDocsAreHonest pins the ExchangeAuth contract on the
// endpoints that DO carry exchanges (Vertex/Bedrock/Azure-class
// surfaces elsewhere in the catalog): every Exchange description
// names its kind, a doc URL, and never promises a flow the resolver
// cannot answer (the resolver returns the description — the caller
// performs the exchange; the catalog documents, it does not fake).
func TestExchangeDocsAreHonest(t *testing.T) {
	count := 0
	for _, p := range All() {
		for _, e := range p.Endpoints {
			if e.Exchange == nil {
				continue
			}
			count++
			if strings.TrimSpace(e.Exchange.Kind) == "" {
				t.Errorf("provider %q endpoint %q: exchange with empty Kind", p.ID, e.Endpoint)
			}
			if strings.TrimSpace(e.Exchange.Description) == "" {
				t.Errorf("provider %q endpoint %q: exchange %q with empty Description — the resolver must be able to tell the caller how the credential is obtained", p.ID, e.Endpoint, e.Exchange.Kind)
			}
		}
	}
	if count == 0 {
		t.Fatal("no Exchange descriptions found in the catalog — the exchange-documentation surface disappeared; reclassify deliberately")
	}
}
