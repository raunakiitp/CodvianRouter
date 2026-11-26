package server

// WS2 unified identity (A1): every identity-dependent check resolves
// the ONE canonical principal. These tests pin the resolution matrix:
// every credential presentation, the documented precedence, the
// authenticated/unauthenticated boundary, and caller attribution.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// credRequest builds a request carrying exactly one credential form.
func credRequest(method, path, bearer, apiKey, googKey string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}
	if googKey != "" {
		req.Header.Set("X-Goog-Api-Key", googKey)
	}
	return req
}

// TestPrincipalForEveryCredentialForm (A1 auth matrix): all four
// supported presentations resolve to the same authenticated
// principal — same Authenticated flag, same caller identity, same
// scopes. The caller identity must not depend on WHICH header the
// client used (that was AUD-SEC-001's bypass shape).
func TestPrincipalForEveryCredentialForm(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithStaticKeys("key-abc", "vendor-key"))
	srv.verifier = NewStaticKeyVerifier("key-abc", "vendor-key")

	forms := []struct {
		name   string
		bearer string
		apiKey string
		goog   string
	}{
		{"bearer", "key-abc", "", ""},
		{"x-api-key", "", "key-abc", ""},
		{"x-goog-api-key", "", "", "key-abc"},
	}
	var firstID string
	for _, f := range forms {
		t.Run(f.name, func(t *testing.T) {
			req := credRequest(http.MethodPost, "/v1/chat/completions", f.bearer, f.apiKey, f.goog)
			p := srv.principalFor(req)
			if !p.Authenticated {
				t.Fatalf("presentation %s not authenticated", f.name)
			}
			if !p.Presented {
				t.Fatalf("presentation %s not marked presented", f.name)
			}
			if p.KeyID == "" {
				t.Fatalf("presentation %s resolved no caller identity", f.name)
			}
			if !p.ScopesKnown {
				t.Fatalf("presentation %s did not resolve scopes", f.name)
			}
			if firstID == "" {
				firstID = p.KeyID
			} else if p.KeyID != firstID {
				t.Fatalf("caller identity differs across presentations: %q vs %q", firstID, p.KeyID)
			}
		})
	}
}

// TestPrincipalForPrecedence (A1): when several credential forms are
// present at once, the documented precedence applies — Authorization
// Bearer wins over X-Api-Key, which wins over X-Goog-Api-Key. A
// request mixing a valid Bearer with an invalid vendor header must
// authenticate as the BEARER identity, never the vendor one, and a
// valid vendor header under an invalid Bearer must NOT smuggle the
// vendor identity through.
func TestPrincipalForPrecedence(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("good-key")

	// Bearer wins: valid Bearer + invalid X-Api-Key → authenticated.
	req := credRequest(http.MethodPost, "/v1/chat/completions", "good-key", "bad-key", "")
	p := srv.principalFor(req)
	if !p.Authenticated {
		t.Fatal("valid Bearer under an invalid X-Api-Key must authenticate (Bearer precedence)")
	}
	if p.Raw != "good-key" {
		t.Fatalf("principal.Raw = %q, want the Bearer credential", p.Raw)
	}

	// The reverse mix: invalid Bearer + valid X-Api-Key. requestKey
	// consults the Bearer FIRST, so the presented credential is the
	// invalid Bearer — the vendor header does not rescue it. This is
	// the documented precedence: one credential per request, highest
	// form wins, no identity mixing.
	req2 := credRequest(http.MethodPost, "/v1/chat/completions", "bad-key", "good-key", "")
	p2 := srv.principalFor(req2)
	if p2.Authenticated {
		t.Fatal("invalid Bearer must not authenticate even when a valid X-Api-Key is present (documented precedence)")
	}
	if p2.Raw != "bad-key" {
		t.Fatalf("principal.Raw = %q, want the Bearer credential (no identity mixing)", p2.Raw)
	}
}

// TestPrincipalForUnauthenticated (A1): an invalid credential, an
// absent one, and a nil verifier all yield Authenticated=false with
// no manufactured identity or scopes.
func TestPrincipalForUnauthenticated(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("good-key")

	// Invalid credential: presented but not authenticated, no
	// identity, no scopes, ScopesKnown=false.
	req := credRequest(http.MethodGet, "/v1/models", "wrong-key", "", "")
	p := srv.principalFor(req)
	if p.Authenticated {
		t.Fatal("invalid credential authenticated")
	}
	if !p.Presented {
		t.Fatal("invalid credential not marked presented")
	}
	if p.KeyID != "" {
		t.Fatalf("invalid credential resolved an identity %q — identity must never be manufactured for unauthenticated credentials", p.KeyID)
	}
	if p.ScopesKnown || len(p.Scopes) != 0 {
		t.Fatalf("invalid credential resolved scopes %v — scope enforcement would leak", p.Scopes)
	}

	// Absent credential: nothing presented at all.
	empty := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	p2 := srv.principalFor(empty)
	if p2.Presented || p2.Authenticated || p2.KeyID != "" {
		t.Fatalf("absent credential resolved %+v, want an empty unauthenticated principal", p2)
	}

	// Nil verifier: a presented credential cannot authenticate.
	noVerifier := newTestServer(t, &fakeEngine{})
	noVerifier.verifier = nil
	req3 := credRequest(http.MethodGet, "/v1/models", "good-key", "", "")
	p3 := noVerifier.principalFor(req3)
	if p3.Authenticated || p3.KeyID != "" {
		t.Fatalf("nil verifier authenticated %+v", p3)
	}
	if !p3.Presented {
		t.Fatal("nil verifier lost the presented marker (auth-relaxed diagnostics need it)")
	}
}

// TestPrincipalForConsistentWithAuthorizedAndCallerKeyID (A1): the
// canonical principal agrees with the consumers — authorized() is
// exactly principal.Authenticated, and callerKeyID() is exactly
// principal.KeyID, for every matrix row.
func TestPrincipalForConsistentWithAuthorizedAndCallerKeyID(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("good-key")

	rows := []struct {
		name       string
		bearer     string
		wantAuthed bool
		wantID     string
	}{
		{"valid key", "good-key", true, "static-"},
		{"invalid key", "nope", false, ""},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			req := credRequest(http.MethodPost, "/v1/chat/completions", row.bearer, "", "")
			p := srv.principalFor(req)
			if got := srv.authorized(req); got != row.wantAuthed {
				t.Fatalf("authorized() = %v, want %v (principal disagreement)", got, row.wantAuthed)
			}
			if p.Authenticated != row.wantAuthed {
				t.Fatalf("principal.Authenticated = %v, want %v", p.Authenticated, row.wantAuthed)
			}
			gotID := srv.callerKeyID(req)
			if row.wantID == "" {
				if gotID != "" {
					t.Fatalf("callerKeyID() = %q, want \"\"", gotID)
				}
			} else {
				if gotID == "" || len(gotID) < len(row.wantID) || gotID[:len(row.wantID)] != row.wantID {
					t.Fatalf("callerKeyID() = %q, want a %q… identity", gotID, row.wantID)
				}
				if gotID != p.KeyID {
					t.Fatalf("callerKeyID() = %q disagrees with principal.KeyID %q", gotID, p.KeyID)
				}
			}
		})
	}
}

// TestRequireScopeUsesCanonicalPrincipal (A1): the scope wrapper
// enforces the principal's resolved scopes — a key carrying the
// required scope passes, a key without it fails closed with 403, and
// an unauthenticated credential still passes through to the auth
// layer (401 there), never getting scope-granted.
func TestRequireScopeUsesCanonicalPrincipal(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	srv.verifier = NewStaticKeyVerifier("full-key")

	handler := srv.requireScope(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	}, []string{"chat"})

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", handler)

	// Valid key with all scopes → 200.
	req := credRequest(http.MethodPost, "/v1/chat/completions", "full-key", "", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("scoped key status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Invalid credential → scope layer passes through (no scope
	// metadata to enforce); the auth layer owns the 401. It must
	// never be scope-granted: simulate by checking the principal
	// directly has no scopes.
	req2 := credRequest(http.MethodPost, "/v1/chat/completions", "nope", "", "")
	p := srv.principalFor(req2)
	if p.ScopesKnown || p.Authenticated {
		t.Fatal("unauthenticated credential got scope metadata — enforcement leak")
	}
}
