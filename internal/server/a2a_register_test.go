// M9.9 integration-A registration tests (internal/server/a2a_card.go
// + server.go + guard.go): the agent card route is registered on a
// live server with the exact card shape, publicly, with the version
// and base URL wired from the server — not from the handler's old
// literals and never from the request.
//
// NOTE: GET /api/a2a/status has NO registration test here yet — it
// is not registered (see the registration note in server.go: the
// status handler needs the A2A *Store and a2a.Service exposes no
// Store accessor). Its registration test lands with the follow-up
// slice that adds that accessor; asserting a 404 for it here would
// be a test the follow-up has to delete.
package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// a2aCardRegisteredServer builds a live server with the card route
// wired: listen address (the card's base-URL source) + version.
func a2aCardRegisteredServer(t *testing.T, listenAddr, version string) *Server {
	t.Helper()
	opts := []Option{WithListenAddr(listenAddr)}
	if version != "" {
		opts = append(opts, WithVersion(version))
	}
	return newTestServer(t, &fakeEngine{}, opts...)
}

// decodeRegisteredCard GETs the card off a live server and decodes
// it into a generic map.
func decodeRegisteredCard(t *testing.T, srv *Server, key string) (int, map[string]json.RawMessage, string) {
	t.Helper()
	rec := doReq(t, srv, http.MethodGet, "/.well-known/agent.json", "", key)
	if rec.Code != http.StatusOK {
		return rec.Code, nil, rec.Body.String()
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("card is not valid JSON: %v (body %q)", err, rec.Body.String())
	}
	return rec.Code, doc, rec.Body.String()
}

func TestA2AAgentCardRegistered(t *testing.T) {
	srv := a2aCardRegisteredServer(t, "127.0.0.1:20128", "9.9.9-test")

	// Public: no gateway key at all — the card is a discovery
	// document and must be fetchable before a client can present
	// a credential. (doReq stamps a non-loopback RemoteAddr, so
	// this also proves the route is not loopback-gated.)
	code, doc, body := decodeRegisteredCard(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json without a key: status %d, want 200 (body %q)", code, body)
	}

	// Exactly the eight A2A fields, nothing else.
	wantKeys := map[string]bool{
		"name": true, "description": true, "url": true, "version": true,
		"defaultInputModes": true, "defaultOutputModes": true,
		"capabilities": true, "skills": true,
	}
	if len(doc) != len(wantKeys) {
		t.Fatalf("card has %d top-level keys, want exactly %d", len(doc), len(wantKeys))
	}
	for k := range doc {
		if !wantKeys[k] {
			t.Fatalf("unexpected top-level key %q in card", k)
		}
	}

	// url is the wired base URL + "/a2a"; version is the wired
	// server version — neither a literal nor request-derived.
	var url, version string
	if err := json.Unmarshal(doc["url"], &url); err != nil || url != "http://127.0.0.1:20128/a2a" {
		t.Fatalf("url %q, want %q", url, "http://127.0.0.1:20128/a2a")
	}
	if err := json.Unmarshal(doc["version"], &version); err != nil || version != "9.9.9-test" {
		t.Fatalf("version %q, want %q (the server's wired version)", version, "9.9.9-test")
	}

	// The live card serves the real skill registry, in order.
	var skills []a2a.SkillDescriptor
	if err := json.Unmarshal(doc["skills"], &skills); err != nil {
		t.Fatalf("skills do not decode: %v", err)
	}
	if want := a2a.SkillDescriptors(); !reflect.DeepEqual(skills, want) {
		t.Fatalf("served skills are not the real registry verbatim:\n got %+v\nwant %+v", skills, want)
	}

	// Wrong method is 405, not a card.
	rec := doReq(t, srv, http.MethodPost, "/.well-known/agent.json", "{}", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /.well-known/agent.json: status %d, want 405", rec.Code)
	}
}

func TestA2AAgentCardGuardRow(t *testing.T) {
	// The guard table classifies the card as Tier MANAGEMENT,
	// auth-exempt, with no scopes (an exempt route must name
	// none it never enforces).
	found := false
	for _, r := range RouteTable() {
		if r.Method == http.MethodGet && r.Path == "/.well-known/agent.json" {
			found = true
			if r.Tier != TierManagement {
				t.Errorf("route tier = %s, want MANAGEMENT", r.Tier)
			}
			if !r.AuthExempt {
				t.Error("route is not auth-exempt; the agent card is a public discovery document")
			}
			if len(r.Scopes) != 0 {
				t.Errorf("route names scopes %v it never enforces", r.Scopes)
			}
			if r.SpawnCapable {
				t.Error("route is spawn-capable; the card serves a static document")
			}
		}
	}
	if !found {
		t.Error("GET /.well-known/agent.json is missing from the guard route table")
	}
}

func TestA2AAgentCardVersionFallback(t *testing.T) {
	// No WithVersion: the card reports "0.1.0", the same
	// fallback GET /openapi.json applies (openapi.go) — never
	// an empty version.
	srv := a2aCardRegisteredServer(t, "127.0.0.1:20128", "")
	code, doc, body := decodeRegisteredCard(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json: status %d, want 200 (body %q)", code, body)
	}
	var version string
	if err := json.Unmarshal(doc["version"], &version); err != nil || version != "0.1.0" {
		t.Fatalf("version %q, want the OpenAPI-style fallback %q", version, "0.1.0")
	}
}

func TestA2AAgentCardBaseURLFromListenAddr(t *testing.T) {
	// A bare ":port" listen address expands to 127.0.0.1, the
	// same expansion handleOpenAPI uses for its servers URL.
	srv := a2aCardRegisteredServer(t, ":20128", "9.9.9-test")
	code, doc, body := decodeRegisteredCard(t, srv, "")
	if code != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json: status %d, want 200 (body %q)", code, body)
	}
	var url string
	if err := json.Unmarshal(doc["url"], &url); err != nil || url != "http://127.0.0.1:20128/a2a" {
		t.Fatalf("url %q, want %q (listen addr :20128 expands to 127.0.0.1)", url, "http://127.0.0.1:20128/a2a")
	}

	// Trailing slash on the listen address is trimmed, never
	// doubled into the card url.
	srv2 := newTestServer(t, &fakeEngine{}, WithListenAddr("127.0.0.1:20128/"))
	if got := srv2.agentCardBaseURL(); got != "http://127.0.0.1:20128" {
		t.Fatalf("agentCardBaseURL with trailing-slash listen addr = %q, want %q", got, "http://127.0.0.1:20128")
	}
}

func TestA2AAgentCardNotRegisteredWithoutListenAddr(t *testing.T) {
	// REQUIRE non-empty base URL at registration: a server with
	// no listen address configured serves no card at all, rather
	// than a card whose url names an address it does not listen
	// on. (Production always sets WithListenAddr from config
	// listen, so the card is always served there.)
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/.well-known/agent.json", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /.well-known/agent.json on a server with no listen address: status %d, want 404 (body %q)", rec.Code, rec.Body.String())
	}
	if got := srv.agentCardBaseURL(); got != "" {
		t.Fatalf("agentCardBaseURL with no listen address = %q, want empty", got)
	}
}
