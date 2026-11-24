// M9.9 A2A Agent Card tests (internal/server/a2a_card.go): real HTTP
// via httptest — no mocks standing in for the wire.
//
// The six skill descriptors are passed literally, in the contract's
// fixed order, for the fixture-based tests below; the real registry
// is covered separately by TestA2AAgentCardRealSkillDescriptors,
// which serves internal/a2a.SkillDescriptors() through the handler.
// The version is injected by each test (testAgentCardVersion) and
// asserted verbatim, so the tests prove the handler serves the
// version it was given — there is no version literal in the
// handler to drift from --version.
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
)

// testAgentCardVersion is the version injected into
// AgentCardHandler by the tests below; every version assertion
// checks the card reports exactly this, proving injection.
const testAgentCardVersion = "9.9.9-test"

// testAgentCardSkills is the contract's twelve skills, in fixed order,
// passed literally to AgentCardHandler.
var testAgentCardSkills = []a2a.SkillDescriptor{
	{ID: "smart-routing", Name: "Smart Routing", Description: "Routes each request to the best provider and model for the task.", Tags: []string{"routing", "providers"}},
	{ID: "quota-management", Name: "Quota Management", Description: "Tracks and enforces quota and budget limits across providers.", Tags: []string{"quota", "budgets"}},
	{ID: "provider-discovery", Name: "Provider Discovery", Description: "Discovers which providers and models are available to route to.", Tags: []string{"providers", "discovery"}},
	{ID: "cost-analysis", Name: "Cost Analysis", Description: "Analyses the cost of requests across providers and models.", Tags: []string{"cost", "pricing"}},
	{ID: "health-report", Name: "Health Report", Description: "Reports the health of providers, models and the router itself.", Tags: []string{"health", "status"}},
	{ID: "list-capabilities", Name: "List Capabilities", Description: "Lists the full catalog of agent skills this router offers.", Tags: []string{"discovery", "capabilities"}},
	{ID: "budget-forecasting", Name: "Budget Forecasting", Description: "Burn-rate forecast for one gateway key.", Tags: []string{"cost", "quota", "forecast"}},
	{ID: "cost-estimation", Name: "Cost Estimation", Description: "Pre-dispatch cost estimate for a model.", Tags: []string{"cost", "estimate"}},
	{ID: "route-preview", Name: "Route Preview", Description: "Dry-run route preview for a model.", Tags: []string{"routing", "preview"}},
	{ID: "compression-analytics", Name: "Compression Analytics", Description: "Live compression analytics.", Tags: []string{"compression", "analytics"}},
	{ID: "eval-management", Name: "Eval Management", Description: "List scheduled evaluation schedules.", Tags: []string{"eval", "schedule"}},
	{ID: "org-reporting", Name: "Org Reporting", Description: "Organization/team reporting summary.", Tags: []string{"org", "reporting"}},
}

// testAgentCardSkillIDs is the contract's fixed skill order.
var testAgentCardSkillIDs = []string{
	"smart-routing",
	"quota-management",
	"provider-discovery",
	"cost-analysis",
	"health-report",
	"list-capabilities",
	"budget-forecasting",
	"cost-estimation",
	"route-preview",
	"compression-analytics",
	"eval-management",
	"org-reporting",
}

// decodeAgentCard GETs the card and decodes it into a generic map
// plus the typed skills list, returning the raw body too.
func decodeAgentCard(t *testing.T, h http.HandlerFunc) (map[string]json.RawMessage, []a2a.SkillDescriptor, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /.well-known/agent.json: status %d, want %d (body %q)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q, want %q", ct, "application/json")
	}
	body := rec.Body.String()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("card is not valid JSON: %v (body %q)", err, body)
	}
	var skills []a2a.SkillDescriptor
	if err := json.Unmarshal(doc["skills"], &skills); err != nil {
		t.Fatalf("skills do not decode: %v", err)
	}
	return doc, skills, body
}

func TestA2AAgentCard(t *testing.T) {
	const baseURL = "http://127.0.0.1:20128"
	h := AgentCardHandler(testAgentCardSkills, baseURL, testAgentCardVersion)
	doc, skills, body := decodeAgentCard(t, h)

	// The card serves ONLY the agent-card document: exactly the
	// eight A2A fields, nothing else (no task data, no keys, no
	// provider details).
	wantKeys := map[string]bool{
		"name": true, "description": true, "url": true, "version": true,
		"defaultInputModes": true, "defaultOutputModes": true,
		"capabilities": true, "skills": true,
	}
	if len(doc) != len(wantKeys) {
		t.Fatalf("card has %d top-level keys %v, want exactly %d %v", len(doc), agentCardKeysOf(doc), len(wantKeys), wantKeys)
	}
	for k := range doc {
		if !wantKeys[k] {
			t.Fatalf("unexpected top-level key %q in card", k)
		}
	}

	var name, description, url, version string
	if err := json.Unmarshal(doc["name"], &name); err != nil || name != "Codvian Router" {
		t.Fatalf("name %q, want %q", name, "Codvian Router")
	}
	if err := json.Unmarshal(doc["description"], &description); err != nil || description == "" {
		t.Fatalf("description is empty or undecodable: %v", err)
	}
	if err := json.Unmarshal(doc["url"], &url); err != nil || url != baseURL+"/a2a" {
		t.Fatalf("url %q, want %q", url, baseURL+"/a2a")
	}
	if err := json.Unmarshal(doc["version"], &version); err != nil || version != testAgentCardVersion {
		t.Fatalf("version %q, want the injected version %q", version, testAgentCardVersion)
	}

	var inModes, outModes []string
	if err := json.Unmarshal(doc["defaultInputModes"], &inModes); err != nil || !reflect.DeepEqual(inModes, []string{"text"}) {
		t.Fatalf("defaultInputModes %v, want [text]", inModes)
	}
	if err := json.Unmarshal(doc["defaultOutputModes"], &outModes); err != nil || !reflect.DeepEqual(outModes, []string{"text"}) {
		t.Fatalf("defaultOutputModes %v, want [text]", outModes)
	}

	var caps struct {
		Streaming         bool `json:"streaming"`
		PushNotifications bool `json:"pushNotifications"`
	}
	if err := json.Unmarshal(doc["capabilities"], &caps); err != nil {
		t.Fatalf("capabilities do not decode: %v", err)
	}
	if !caps.Streaming {
		t.Fatal("capabilities.streaming is false, want true")
	}
	if caps.PushNotifications {
		t.Fatal("capabilities.pushNotifications is true, want false (no push mechanism exists)")
	}

	// All twelve skills, in the contract's fixed order, verbatim.
	if len(skills) != len(testAgentCardSkillIDs) {
		t.Fatalf("got %d skills, want %d", len(skills), len(testAgentCardSkillIDs))
	}
	for i, wantID := range testAgentCardSkillIDs {
		if skills[i].ID != wantID {
			t.Fatalf("skills[%d].id %q, want %q", i, skills[i].ID, wantID)
		}
	}
	if !reflect.DeepEqual(skills, testAgentCardSkills) {
		t.Fatalf("skills are not the passed-in descriptors verbatim:\n got %+v\nwant %+v", skills, testAgentCardSkills)
	}

	// No secret material anywhere in the body: no credential-looking
	// values. (Deliberately not the bare word "key": the card
	// legitimately contains no key material, but a substring check
	// on "key" would also match innocent English.)
	for _, secret := range []string{"gk_", "api_key", "apikey", "apiKey", "sk-", "secret", "password", "Authorization", "Bearer", `"token"`} {
		if strings.Contains(body, secret) {
			t.Fatalf("card body contains credential-looking substring %q: %s", secret, body)
		}
	}
}

func TestA2AAgentCardSnapshotDeepCopiesTags(t *testing.T) {
	// Nit 1 regression: the handler snapshots the descriptors, so
	// mutating the caller's Tags after handler creation must not
	// change what the card serves. (A shallow append copy shares
	// each Tags backing array with the caller; the probe that
	// caught this mutated skills[0].Tags[0] from "orig" to
	// "MUTATED" and saw the served card change.)
	skills := []a2a.SkillDescriptor{
		{ID: "smart-routing", Name: "Smart Routing", Description: "Routes each request to the best provider and model for the task.", Tags: []string{"orig", "routing"}},
	}
	h := AgentCardHandler(skills, "http://127.0.0.1:20128", testAgentCardVersion)
	_, before, _ := decodeAgentCard(t, h)
	if !reflect.DeepEqual(before[0].Tags, []string{"orig", "routing"}) {
		t.Fatalf("served tags before mutation %v, want [orig routing]", before[0].Tags)
	}
	skills[0].Tags[0] = "MUTATED"
	_, after, _ := decodeAgentCard(t, h)
	if !reflect.DeepEqual(after[0].Tags, []string{"orig", "routing"}) {
		t.Fatalf("served tags after caller mutated Tags[0] %v, want [orig routing] — snapshot shares the caller's Tags backing array", after[0].Tags)
	}
}

func TestA2AAgentCardRealSkillDescriptors(t *testing.T) {
	// Nit 3 integration test: serve the REAL registry
	// (a2a.SkillDescriptors()) through the handler, so the
	// verbatim and secret-scan checks cover the real descriptors,
	// not the paraphrased fixtures above.
	want := a2a.SkillDescriptors()
	h := AgentCardHandler(want, "http://127.0.0.1:20128", testAgentCardVersion)
	_, skills, body := decodeAgentCard(t, h)

	// Exactly the 6 real skills, in the registry's order.
	if len(skills) != len(want) {
		t.Fatalf("got %d skills, want %d (the real registry)", len(skills), len(want))
	}
	for i, wantDesc := range want {
		if skills[i].ID != wantDesc.ID {
			t.Fatalf("skills[%d].id %q, want %q", i, skills[i].ID, wantDesc.ID)
		}
	}
	if !reflect.DeepEqual(skills, want) {
		t.Fatalf("served skills are not the real registry descriptors verbatim:\n got %+v\nwant %+v", skills, want)
	}

	// No secret material anywhere in the body: no credential-looking
	// values. (Same list as TestA2AAgentCard; deliberately not the
	// bare word "key".)
	for _, secret := range []string{"gk_", "api_key", "apikey", "apiKey", "sk-", "secret", "password", "Authorization", "Bearer", `"token"`} {
		if strings.Contains(body, secret) {
			t.Fatalf("card body contains credential-looking substring %q: %s", secret, body)
		}
	}
}

func TestA2AAgentCardMethodNotAllowed(t *testing.T) {
	h := AgentCardHandler(testAgentCardSkills, "http://127.0.0.1:20128", testAgentCardVersion)
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodHead} {
		req := httptest.NewRequest(method, "/.well-known/agent.json", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status %d, want %d", method, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestA2AAgentCardEmptySkills(t *testing.T) {
	// No skills configured: the card still serves, with an empty
	// array — never null, never an error.
	h := AgentCardHandler(nil, "http://127.0.0.1:20128", testAgentCardVersion)
	_, skills, body := decodeAgentCard(t, h)
	if len(skills) != 0 {
		t.Fatalf("got %d skills, want 0", len(skills))
	}
	if !strings.Contains(body, `"skills":[]`) {
		t.Fatalf("body renders skills as %q, want an empty array, not null", body)
	}
}

func TestA2AAgentCardVersionIsInjected(t *testing.T) {
	// Focused form of the version check: the card's version is
	// exactly the version the caller injected — no literal in
	// the handler, nothing to drift from --version. A distinct
	// injected value proves the handler serves its input, not
	// a hard-coded string.
	h := AgentCardHandler(nil, "http://127.0.0.1:20128", "7.7.7-injected")
	req := httptest.NewRequest(http.MethodGet, "/.well-known/agent.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("card is not valid JSON: %v", err)
	}
	if doc.Version != "7.7.7-injected" {
		t.Fatalf("card version %q, want the injected version %q", doc.Version, "7.7.7-injected")
	}
}

func agentCardKeysOf(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
