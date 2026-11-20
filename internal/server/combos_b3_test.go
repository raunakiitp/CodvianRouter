// B3 tests: duplicate / import / export / test-run /
// enable-disable for the combos API (M10-5 enabler). Every test
// runs against ComboStore.Handler() directly (no auth — the
// coordinator owns registration); the group-mirror checks prove
// enable/disable changes what actually routes, not just the
// stored flag.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// comboB3Seed creates "mock-src" (priority is NOT used here:
// cheapest over mock-upstream/mock-model@low + mock-local) and
// returns the store config so tests can read the group mirror.
func comboB3Setup(t *testing.T) (*config.Config, *ComboStore) {
	t.Helper()
	cfg := comboTestConfig()
	store := NewComboStore(cfg, nil)
	h := store.Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"mock-src","chain":["mock-upstream/mock-model@low","mock-local"],"strategy":"cheapest"}`)
	if status != http.StatusCreated {
		t.Fatalf("seed mock-src: status %d, body %s", status, body)
	}
	return cfg, store
}

func TestComboB3DuplicateRoundTrip(t *testing.T) {
	cfg, store := comboB3Setup(t)
	h := store.Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos/mock-src/duplicate", `{"name":"mock-copy"}`)
	if status != http.StatusCreated {
		t.Fatalf("duplicate: status %d, body %s", status, body)
	}
	var got Combo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("duplicate decode: %v", err)
	}
	if got.ID != "mock-copy" || got.Name != "mock-copy" {
		t.Fatalf("duplicate: id/name = %q/%q, want mock-copy", got.ID, got.Name)
	}
	if got.Strategy != "cheapest" || !got.Enabled || !got.Usable {
		t.Fatalf("duplicate: strategy=%q enabled=%v usable=%v, want cheapest/true/true", got.Strategy, got.Enabled, got.Usable)
	}
	if len(got.Chain) != 2 || got.Chain[0].Provider != "mock-upstream" || got.Chain[0].Model != "mock-model" || got.Chain[0].Effort != "low" || got.Chain[1].Provider != "mock-local" {
		t.Fatalf("duplicate: chain = %+v, want the source's steps verbatim", got.Chain)
	}
	// The copy routes immediately, like a create: group +
	// strategy mirrors hold the copied chain.
	if members := cfg.Routing.Groups["mock-copy"]; len(members) != 2 || members[0] != "mock-upstream@low" || members[1] != "mock-local" {
		t.Fatalf("duplicate: group mirror = %v, want [mock-upstream@low mock-local]", members)
	}
	if cfg.Routing.ChainStrategies["mock-copy"] != "cheapest" {
		t.Fatalf("duplicate: strategy mirror = %q, want cheapest", cfg.Routing.ChainStrategies["mock-copy"])
	}
	// Source untouched.
	status, body = comboDo(t, h, "GET", "/v1/combos/mock-src", "")
	if status != http.StatusOK {
		t.Fatalf("get source after duplicate: status %d, body %s", status, body)
	}
}

func TestComboB3DuplicateErrors(t *testing.T) {
	_, store := comboB3Setup(t)
	h := store.Handler()

	cases := []struct {
		name       string
		path       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{"missing name", "/v1/combos/mock-src/duplicate", `{}`, http.StatusBadRequest, "name:"},
		{"empty name", "/v1/combos/mock-src/duplicate", `{"name":""}`, http.StatusBadRequest, "name:"},
		{"colliding name", "/v1/combos/mock-src/duplicate", `{"name":"mock-src"}`, http.StatusConflict, "already exists"},
		{"unknown source", "/v1/combos/no-such/duplicate", `{"name":"x"}`, http.StatusNotFound, "not found"},
		{"unknown field", "/v1/combos/mock-src/duplicate", `{"name":"x","bogus":1}`, http.StatusBadRequest, "unknown field"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := comboDo(t, h, "POST", tc.path, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status %d, body %s (want %d)", status, body, tc.wantStatus)
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Fatalf("body %s does not contain %q", body, tc.wantSubstr)
			}
		})
	}
}

func TestComboB3DuplicateDisabledStaysDisabled(t *testing.T) {
	cfg, store := comboB3Setup(t)
	h := store.Handler()

	status, _ := comboDo(t, h, "POST", "/v1/combos/mock-src/disable", "")
	if status != http.StatusOK {
		t.Fatalf("disable source: status %d", status)
	}
	status, body := comboDo(t, h, "POST", "/v1/combos/mock-src/duplicate", `{"name":"mock-copy"}`)
	if status != http.StatusCreated {
		t.Fatalf("duplicate disabled: status %d, body %s", status, body)
	}
	var got Combo
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("duplicate decode: %v", err)
	}
	if got.Enabled {
		t.Fatalf("duplicate of a disabled combo: enabled=true, want inherited disabled")
	}
	if _, ok := cfg.Routing.Groups["mock-copy"]; ok {
		t.Fatalf("duplicate of a disabled combo: group mirror exists — a disabled copy must not route")
	}
}

func TestComboB3ExportImportRoundTrip(t *testing.T) {
	cfg := comboTestConfig()
	store := NewComboStore(cfg, nil)
	h := store.Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"orig","chain":[{"provider":"mock-upstream","model":"mock-model"},{"provider":"mock-local","effort":"low"}],"strategy":"cheapest"}`)
	if status != http.StatusCreated {
		t.Fatalf("create orig: status %d, body %s", status, body)
	}

	// Export one combo: the shape must carry no derived facts.
	status, body = comboDo(t, h, "GET", "/v1/combos/orig/export", "")
	if status != http.StatusOK {
		t.Fatalf("export: status %d, body %s", status, body)
	}
	var exported ComboExport
	if err := json.Unmarshal(body, &exported); err != nil {
		t.Fatalf("export decode: %v", err)
	}
	if exported.Name != "orig" || exported.Strategy != "cheapest" || !exported.Enabled || len(exported.Chain) != 2 {
		t.Fatalf("export: %+v, want orig/cheapest/enabled/2 steps", exported)
	}
	if exported.Chain[0].Model != "mock-model" || exported.Chain[1].Effort != "low" || exported.Chain[1].Model != "" {
		t.Fatalf("export chain: %+v, want the stored steps verbatim", exported.Chain)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("export raw decode: %v", err)
	}
	chain0 := raw["chain"].([]any)[0].(map[string]any)
	if _, ok := chain0["has_key"]; ok {
		t.Fatalf("export chain step carries has_key — derived facts must not be exported (import would reject them)")
	}
	if _, ok := chain0["usable"]; ok {
		t.Fatalf("export chain step carries usable — derived facts must not be exported")
	}

	// Export -> import under a new name: identical members/strategy.
	exported.Name = "restored"
	rebodied, _ := json.Marshal(exported)
	status, body = comboDo(t, h, "POST", "/v1/combos/import", string(rebodied))
	if status != http.StatusCreated {
		t.Fatalf("import as restored: status %d, body %s", status, body)
	}
	status, body = comboDo(t, h, "GET", "/v1/combos/restored", "")
	if status != http.StatusOK {
		t.Fatalf("get restored: status %d, body %s", status, body)
	}
	var restored Combo
	if err := json.Unmarshal(body, &restored); err != nil {
		t.Fatalf("restored decode: %v", err)
	}
	if restored.Strategy != "cheapest" || !restored.Enabled || len(restored.Chain) != 2 ||
		restored.Chain[0].Provider != "mock-upstream" || restored.Chain[0].Model != "mock-model" ||
		restored.Chain[1].Provider != "mock-local" || restored.Chain[1].Effort != "low" {
		t.Fatalf("restored: %+v, want orig's members/strategy verbatim", restored)
	}

	// Export -> delete -> import the SAME name verbatim: the
	// original export bytes round-trip without renaming.
	status, body = comboDo(t, h, "GET", "/v1/combos/orig/export", "")
	if status != http.StatusOK {
		t.Fatalf("re-export orig: status %d", status)
	}
	origExport := body
	status, _ = comboDo(t, h, "DELETE", "/v1/combos/orig", "")
	if status != http.StatusOK {
		t.Fatalf("delete orig: status %d", status)
	}
	status, body = comboDo(t, h, "POST", "/v1/combos/import", string(origExport))
	if status != http.StatusCreated {
		t.Fatalf("import original export after delete: status %d, body %s", status, body)
	}
	status, body = comboDo(t, h, "GET", "/v1/combos/orig", "")
	if status != http.StatusOK {
		t.Fatalf("get re-imported orig: status %d, body %s", status, body)
	}
}

func TestComboB3ImportCollision409(t *testing.T) {
	_, store := comboB3Setup(t)
	h := store.Handler()

	status, body := comboDo(t, h, "GET", "/v1/combos/mock-src/export", "")
	if status != http.StatusOK {
		t.Fatalf("export: status %d", status)
	}
	// Same name, still present: 409, never a silent overwrite.
	status, body = comboDo(t, h, "POST", "/v1/combos/import", string(body))
	if status != http.StatusConflict {
		t.Fatalf("import collision: status %d, body %s (want 409)", status, body)
	}
	if !strings.Contains(string(body), "already exists") {
		t.Fatalf("import collision body %s does not say already exists", body)
	}
	// The original is untouched by the failed import.
	status, body = comboDo(t, h, "GET", "/v1/combos/mock-src", "")
	if status != http.StatusOK || comboDecode(t, body)["strategy"] != "cheapest" {
		t.Fatalf("source after colliding import: status %d, body %s", status, body)
	}
}

func TestComboB3ImportInvalid400(t *testing.T) {
	h := comboTestStore().Handler()

	cases := []struct {
		name       string
		body       string
		wantSubstr string
	}{
		{"unknown provider", `{"name":"bad1","chain":[{"provider":"nope"}],"strategy":"priority"}`, "unknown provider"},
		{"unknown model", `{"name":"bad2","chain":[{"provider":"mock-upstream","model":"nope"}],"strategy":"priority"}`, "unknown model"},
		{"unknown strategy", `{"name":"bad3","chain":[{"provider":"mock-upstream"}],"strategy":"nope"}`, "unknown strategy"},
		{"empty chain", `{"name":"bad4","chain":[],"strategy":"priority"}`, "must not be empty"},
		{"unknown field", `{"name":"bad5","chain":[{"provider":"mock-upstream"}],"strategy":"priority","bogus":1}`, "unknown field"},
		{"unknown chain field", `{"name":"bad6","chain":[{"provider":"mock-upstream","bogus":1}],"strategy":"priority"}`, "unknown field"},
		{"envelope not one combo", `{"combos":[{"name":"a","chain":[{"provider":"mock-upstream"}],"strategy":"priority"},{"name":"b","chain":[{"provider":"mock-upstream"}],"strategy":"priority"}]}`, "one at a time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := comboDo(t, h, "POST", "/v1/combos/import", tc.body)
			if status != http.StatusBadRequest {
				t.Fatalf("import: status %d, body %s (want 400)", status, body)
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Fatalf("import body %s does not contain %q", body, tc.wantSubstr)
			}
			// The same body must fail create identically: import
			// reuses the create validation path, it does not
			// have its own (weaker or stricter) rules.
			if tc.name != "envelope not one combo" {
				status, _ := comboDo(t, h, "POST", "/v1/combos", tc.body)
				if status != http.StatusBadRequest {
					t.Fatalf("create with the same body: status %d (want 400, matching import)", status)
				}
			}
		})
	}
}

func TestComboB3ExportAll(t *testing.T) {
	_, store := comboB3Setup(t)
	h := store.Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"second","chain":["mock-local"],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("create second: status %d, body %s", status, body)
	}
	status, body = comboDo(t, h, "GET", "/v1/combos/export", "")
	if status != http.StatusOK {
		t.Fatalf("export all: status %d, body %s", status, body)
	}
	var out struct {
		Combos []ComboExport `json:"combos"`
		Count  int           `json:"count"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("export all decode: %v", err)
	}
	if out.Count != 2 || len(out.Combos) != 2 {
		t.Fatalf("export all: count=%d len=%d, want 2", out.Count, len(out.Combos))
	}
	if out.Combos[0].Name != "mock-src" || out.Combos[1].Name != "second" {
		t.Fatalf("export all: names not sorted: %+v", out.Combos)
	}
	// Every entry is itself importable (under a new name).
	for _, e := range out.Combos {
		e.Name += "-copy"
		b, _ := json.Marshal(e)
		status, body := comboDo(t, h, "POST", "/v1/combos/import", string(b))
		if status != http.StatusCreated {
			t.Fatalf("import export-all entry %q: status %d, body %s", e.Name, status, body)
		}
	}
}

func TestComboB3ImportDisabledStaysUnmirrored(t *testing.T) {
	cfg := comboTestConfig()
	store := NewComboStore(cfg, nil)
	h := store.Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos/import", `{"name":"off","chain":["mock-upstream"],"strategy":"priority","enabled":false}`)
	if status != http.StatusCreated {
		t.Fatalf("import disabled: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["enabled"] != false {
		t.Fatalf("import disabled: enabled=%v, want false", got["enabled"])
	}
	if _, ok := cfg.Routing.Groups["off"]; ok {
		t.Fatalf("import disabled: group mirror exists — a disabled import must not route")
	}
	// Enabling it mirrors the stored chain verbatim.
	status, _ = comboDo(t, h, "POST", "/v1/combos/off/enable", "")
	if status != http.StatusOK {
		t.Fatalf("enable imported: status %d", status)
	}
	if members := cfg.Routing.Groups["off"]; len(members) != 1 || members[0] != "mock-upstream" {
		t.Fatalf("enable imported: group mirror = %v, want [mock-upstream]", members)
	}
}

func TestComboB3TestRunSimulation(t *testing.T) {
	_, store := comboB3Setup(t)
	h := store.Handler()

	// mock-src is usable; add a combo with a keyless (unusable)
	// middle step to prove the simulation reports, not hides,
	// the step that would fail.
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"sim","chain":["mock-upstream/mock-model","mock-keyless","mock-local"],"strategy":"cheapest"}`)
	if status != http.StatusCreated {
		t.Fatalf("create sim: status %d, body %s", status, body)
	}

	status, body = comboDo(t, h, "POST", "/v1/combos/sim/test-run", "")
	if status != http.StatusOK {
		t.Fatalf("test-run: status %d, body %s", status, body)
	}
	var run ComboTestRun
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatalf("test-run decode: %v", err)
	}
	// The no-spend guarantee, in-band: nothing was called, no
	// quota moved. (The handler touches no provider client and
	// no ledger; these fields are the response's own proof.)
	if run.WouldCallUpstream || run.UpstreamCalls != 0 || run.QuotaSpent {
		t.Fatalf("test-run: would_call_upstream=%v upstream_calls=%d quota_spent=%v — a simulation must never spend", run.WouldCallUpstream, run.UpstreamCalls, run.QuotaSpent)
	}
	if !run.WouldServe {
		t.Fatalf("test-run: would_serve=false, want true (two of three steps are usable)")
	}
	if run.Strategy != "cheapest" || !run.Enabled {
		t.Fatalf("test-run: strategy=%q enabled=%v, want cheapest/true", run.Strategy, run.Enabled)
	}
	if !strings.Contains(run.Note, "no provider was called") {
		t.Fatalf("test-run note does not state no provider was called: %q", run.Note)
	}
	if len(run.Attempts) != 3 {
		t.Fatalf("test-run: %d attempts, want 3 in declared order", len(run.Attempts))
	}
	a0, a1, a2 := run.Attempts[0], run.Attempts[1], run.Attempts[2]
	if a0.Provider != "mock-upstream" || !a0.Usable || a0.ModelVerified == nil || !*a0.ModelVerified {
		t.Fatalf("test-run step 0: %+v, want usable mock-upstream with a verified model pin", a0)
	}
	if !strings.Contains(a0.OnFailure, "fail over to step 1") {
		t.Fatalf("test-run step 0 on_failure: %q", a0.OnFailure)
	}
	if a1.Provider != "mock-keyless" || a1.Usable || a1.UnusableReason != "provider has no key" {
		t.Fatalf("test-run step 1: %+v, want unusable keyless with its reason", a1)
	}
	if a2.Provider != "mock-local" || !a2.Usable || a2.Model != "" {
		t.Fatalf("test-run step 2: %+v, want usable mock-local with no effective model", a2)
	}
	if !strings.Contains(a2.OnFailure, "no further steps") {
		t.Fatalf("test-run step 2 on_failure: %q", a2.OnFailure)
	}

	// A requested model applies to unpinned steps only: the
	// pinned step keeps mock-model, the local step takes it.
	status, body = comboDo(t, h, "POST", "/v1/combos/sim/test-run", `{"model":"mock-model"}`)
	if status != http.StatusOK {
		t.Fatalf("test-run with model: status %d, body %s", status, body)
	}
	if err := json.Unmarshal(body, &run); err != nil {
		t.Fatalf("test-run decode: %v", err)
	}
	if run.Model != "mock-model" || run.Attempts[0].Model != "mock-model" || run.Attempts[2].Model != "mock-model" {
		t.Fatalf("test-run with model: model=%q attempts=%+v", run.Model, run.Attempts)
	}
	if run.Attempts[2].ModelVerified == nil || !*run.Attempts[2].ModelVerified {
		t.Fatalf("test-run with model: local step model_verified not true: %+v", run.Attempts[2])
	}

	// Unknown combo, and an unknown field, both fail honestly.
	status, _ = comboDo(t, h, "POST", "/v1/combos/no-such/test-run", "")
	if status != http.StatusNotFound {
		t.Fatalf("test-run unknown: status %d (want 404)", status)
	}
	status, body = comboDo(t, h, "POST", "/v1/combos/sim/test-run", `{"prompt_tokens":100}`)
	if status != http.StatusBadRequest {
		t.Fatalf("test-run prompt_tokens: status %d (want 400 — cost cannot be estimated here)", status)
	}
	if !strings.Contains(string(body), "unknown field") {
		t.Fatalf("test-run prompt_tokens body %s does not say unknown field", body)
	}
}

func TestComboB3TestRunDisabled409(t *testing.T) {
	_, store := comboB3Setup(t)
	h := store.Handler()

	status, _ := comboDo(t, h, "POST", "/v1/combos/mock-src/disable", "")
	if status != http.StatusOK {
		t.Fatalf("disable: status %d", status)
	}
	status, body := comboDo(t, h, "POST", "/v1/combos/mock-src/test-run", "")
	if status != http.StatusConflict {
		t.Fatalf("test-run disabled: status %d, body %s (want 409)", status, body)
	}
	if !strings.Contains(string(body), "disabled") {
		t.Fatalf("test-run disabled body %s does not say disabled", body)
	}
	status, body = comboDo(t, h, "POST", "/v1/combos/mock-src/enable", "")
	if status != http.StatusOK {
		t.Fatalf("re-enable: status %d", status)
	}
	status, _ = comboDo(t, h, "POST", "/v1/combos/mock-src/test-run", "")
	if status != http.StatusOK {
		t.Fatalf("test-run after re-enable: status %d (want 200)", status)
	}
}

func TestComboB3EnableDisableRouting(t *testing.T) {
	cfg, store := comboB3Setup(t)
	h := store.Handler()

	// Sanity: the seeded combo routes (both mirrors present).
	if members := cfg.Routing.Groups["mock-src"]; len(members) != 2 {
		t.Fatalf("setup: group mirror = %v, want 2 members", members)
	}
	if cfg.Routing.ChainStrategies["mock-src"] != "cheapest" {
		t.Fatalf("setup: strategy mirror = %q, want cheapest", cfg.Routing.ChainStrategies["mock-src"])
	}

	// Disable: the record stays, the mirrors go — "group:mock-src"
	// stops routing. Usable flips false with the flag, because a
	// disabled combo would serve nothing if asked.
	status, body := comboDo(t, h, "POST", "/v1/combos/mock-src/disable", "")
	if status != http.StatusOK {
		t.Fatalf("disable: status %d, body %s", status, body)
	}
	got := comboDecode(t, body)
	if got["enabled"] != false || got["usable"] != false {
		t.Fatalf("disable: enabled=%v usable=%v, want false/false", got["enabled"], got["usable"])
	}
	if _, ok := cfg.Routing.Groups["mock-src"]; ok {
		t.Fatalf("disable: group mirror still holds mock-src — disabled must mean not routing")
	}
	if _, ok := cfg.Routing.ChainStrategies["mock-src"]; ok {
		t.Fatalf("disable: strategy mirror still holds mock-src")
	}
	// The record itself survives the disable (chain + strategy
	// intact) — GET still finds it.
	status, body = comboDo(t, h, "GET", "/v1/combos/mock-src", "")
	if status != http.StatusOK {
		t.Fatalf("get disabled: status %d", status)
	}
	var kept Combo
	if err := json.Unmarshal(body, &kept); err != nil {
		t.Fatalf("get disabled decode: %v", err)
	}
	if len(kept.Chain) != 2 || kept.Strategy != "cheapest" {
		t.Fatalf("get disabled: chain/strategy changed: %+v", kept)
	}

	// Disabling twice is a 200 no-op, not an error.
	status, _ = comboDo(t, h, "POST", "/v1/combos/mock-src/disable", "")
	if status != http.StatusOK {
		t.Fatalf("second disable: status %d (want 200, idempotent)", status)
	}

	// Enable: both mirrors come back verbatim.
	status, body = comboDo(t, h, "POST", "/v1/combos/mock-src/enable", "")
	if status != http.StatusOK {
		t.Fatalf("enable: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["enabled"] != true || got["usable"] != true {
		t.Fatalf("enable: enabled=%v usable=%v, want true/true", got["enabled"], got["usable"])
	}
	if members := cfg.Routing.Groups["mock-src"]; len(members) != 2 || members[0] != "mock-upstream@low" || members[1] != "mock-local" {
		t.Fatalf("enable: group mirror = %v, want the stored chain back verbatim", members)
	}
	if cfg.Routing.ChainStrategies["mock-src"] != "cheapest" {
		t.Fatalf("enable: strategy mirror = %q, want cheapest", cfg.Routing.ChainStrategies["mock-src"])
	}

	// Unknown ids answer 404, never a silent success.
	status, _ = comboDo(t, h, "POST", "/v1/combos/no-such/enable", "")
	if status != http.StatusNotFound {
		t.Fatalf("enable unknown: status %d (want 404)", status)
	}
	status, _ = comboDo(t, h, "POST", "/v1/combos/no-such/disable", "")
	if status != http.StatusNotFound {
		t.Fatalf("disable unknown: status %d (want 404)", status)
	}
}

func TestComboB3PatchEnabled(t *testing.T) {
	cfg, store := comboB3Setup(t)
	h := store.Handler()

	// PATCH can flip the flag too — same mirrors, same result.
	status, body := comboDo(t, h, "PATCH", "/v1/combos/mock-src", `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("patch disable: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["enabled"] != false {
		t.Fatalf("patch disable: enabled=%v, want false", got["enabled"])
	}
	if _, ok := cfg.Routing.Groups["mock-src"]; ok {
		t.Fatalf("patch disable: group mirror still present")
	}

	// Patching the chain of a disabled combo does NOT re-activate
	// it: the new chain is stored, the mirror stays absent.
	status, body = comboDo(t, h, "PATCH", "/v1/combos/mock-src", `{"chain":["mock-local"]}`)
	if status != http.StatusOK {
		t.Fatalf("patch chain while disabled: status %d, body %s", status, body)
	}
	if _, ok := cfg.Routing.Groups["mock-src"]; ok {
		t.Fatalf("patch chain while disabled: group mirror reappeared — a chain edit must not re-activate a disabled combo")
	}

	// PATCH can flip it back on, with a strategy change in the
	// same body — the new chain + strategy mirror together.
	status, body = comboDo(t, h, "PATCH", "/v1/combos/mock-src", `{"enabled":true,"strategy":"priority"}`)
	if status != http.StatusOK {
		t.Fatalf("patch re-enable: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["enabled"] != true || got["strategy"] != "priority" {
		t.Fatalf("patch re-enable: body %s", body)
	}
	if members := cfg.Routing.Groups["mock-src"]; len(members) != 1 || members[0] != "mock-local" {
		t.Fatalf("patch re-enable: group mirror = %v, want [mock-local]", members)
	}
	if cfg.Routing.ChainStrategies["mock-src"] != "priority" {
		t.Fatalf("patch re-enable: strategy mirror = %q, want priority", cfg.Routing.ChainStrategies["mock-src"])
	}
}
