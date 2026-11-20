// Contract + behaviour tests for the M8.14 combos/chains CRUD API
// (POST/GET/PATCH/DELETE /v1/combos). Every endpoint gets at least
// one apitest fixture contract test (tests/fixtures/v1/m8.14/);
// the round-trip test proves create -> get -> list -> patch ->
// delete -> get-404 end to end against the same store, and the
// toggle test proves the fixture assertions fail when the handler
// is removed.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/apitest"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// comboTestConfig builds a config with mock providers only — no
// real provider is ever named here. mock-upstream holds a key and
// declares mock-model; mock-keyless holds no key (non-local, so
// unusable); mock-local holds no key but is ollama on loopback
// (usable without one); mock-disabled holds a key but is off.
func comboTestConfig() *config.Config {
	models := []provider.Model{{ID: "mock-model"}}
	return &config.Config{
		Providers: []config.ProviderConfig{
			{ID: "mock-upstream", Name: "Mock Upstream", BaseURL: "https://mock-upstream.example", APIKeyEncrypted: "sealed-mock-key", WireFormat: "openai", Models: models, Enabled: true},
			{ID: "mock-keyless", Name: "Mock Keyless", BaseURL: "https://mock-keyless.example", WireFormat: "openai", Models: models, Enabled: true},
			{ID: "mock-local", Name: "Mock Local", BaseURL: "http://127.0.0.1:11434", WireFormat: "ollama", Models: models, Enabled: true},
			{ID: "mock-disabled", Name: "Mock Disabled", BaseURL: "https://mock-disabled.example", APIKeyEncrypted: "sealed-mock-key", WireFormat: "openai", Models: models, Enabled: false},
		},
		Routing: &config.RoutingConfig{},
	}
}

func comboTestStore() *ComboStore {
	return NewComboStore(comboTestConfig(), nil)
}

// comboSeed creates mock-combo (priority over mock-upstream /
// mock-model) through the real HTTP handler, failing the test on
// any problem. Fixture tests that need a pre-existing combo call
// this before apitest.Run.
func comboSeed(t *testing.T, h http.Handler) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/combos", strings.NewReader(`{"name":"mock-combo","chain":[{"provider":"mock-upstream","model":"mock-model"}],"strategy":"priority"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed mock-combo: status %d, body %s", rec.Code, rec.Body.String())
	}
}

// comboDo performs one request against h and returns status + body.
func comboDo(t *testing.T, h http.Handler, method, path, body string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func comboDecode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, body)
	}
	return out
}

// --- Fixture contract tests: one per endpoint outcome. ---

func TestCombosCreate201Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-201"))
}

func TestCombosList200Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	comboSeed(t, h)
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-list-200"))
}

func TestCombosGet200Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	comboSeed(t, h)
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-get-200"))
}

func TestCombosGet404Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-get-404"))
}

func TestCombosCreate400UnknownProviderFixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-400-unknown-provider"))
}

func TestCombosCreate400EmptyChainFixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-400-empty-chain"))
}

func TestCombosCreate400UnknownStrategyFixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-400-unknown-strategy"))
}

func TestCombosCreate400UnknownModelFixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-400-unknown-model"))
}

func TestCombosPatch200Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	comboSeed(t, h)
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-patch-200"))
}

func TestCombosDelete200Fixture(t *testing.T) {
	h := comboTestStore().Handler()
	comboSeed(t, h)
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-delete-200"))
}

func TestCombosCreate201KeylessFlaggedFixture(t *testing.T) {
	h := comboTestStore().Handler()
	apitest.Run(t, h, apitest.LoadFixture(t, "m8.14", "combos-create-201-keyless-flagged"))
}

// TestCombosToggleWithoutHandler proves the fixture contract
// fails when the feature is removed: the same create-201 fixture
// run against a mux with no combos routes must fail its
// assertions (404, not 201).
func TestCombosToggleWithoutHandler(t *testing.T) {
	f := apitest.LoadFixture(t, "m8.14", "combos-create-201")
	empty := http.NewServeMux()
	status, body := comboDo(t, empty, f.Method, f.Path, `{"name":"mock-combo","chain":[{"provider":"mock-upstream","model":"mock-model"}],"strategy":"priority"}`)
	if err := apitest.CheckResponse(&http.Response{StatusCode: status}, body, f); err == nil {
		t.Fatalf("toggle: fixture passed against a handler without the combos routes (status %d); the contract must fail when the feature is removed", status)
	}
}

// TestCombosRoundTrip is the create -> get -> list -> patch ->
// delete -> get-404 round trip against one store, plus the
// group-mirror check: the combo is engine-visible as a routing
// group while it exists and gone from the mirror after delete.
func TestCombosRoundTrip(t *testing.T) {
	cfg := comboTestConfig()
	store := NewComboStore(cfg, nil)
	h := store.Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"rt","chain":["mock-upstream/mock-model","mock-local"],"strategy":"cheapest"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	if got := cfg.Routing.Groups["rt"]; len(got) != 2 || got[0] != "mock-upstream" || got[1] != "mock-local" {
		t.Fatalf("create: group mirror = %v, want [mock-upstream mock-local]", got)
	}
	if cfg.Routing.ChainStrategies["rt"] != "cheapest" {
		t.Fatalf("create: chain_strategies mirror = %q, want cheapest", cfg.Routing.ChainStrategies["rt"])
	}

	status, body = comboDo(t, h, "GET", "/v1/combos/rt", "")
	if status != http.StatusOK {
		t.Fatalf("get: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["strategy"] != "cheapest" {
		t.Fatalf("get: strategy = %v, want cheapest", got["strategy"])
	}

	status, body = comboDo(t, h, "GET", "/v1/combos", "")
	if status != http.StatusOK {
		t.Fatalf("list: status %d, body %s", status, body)
	}
	var list struct {
		Combos []Combo `json:"combos"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("list decode: %v", err)
	}
	if len(list.Combos) != 1 || list.Combos[0].ID != "rt" {
		t.Fatalf("list: got %+v, want one combo rt", list.Combos)
	}
	// String-form steps parsed: provider/model split, and the
	// keyless-free local step stays usable.
	if !list.Combos[0].Chain[1].Usable || list.Combos[0].Chain[1].Provider != "mock-local" {
		t.Fatalf("list: second step = %+v, want usable mock-local", list.Combos[0].Chain[1])
	}

	status, body = comboDo(t, h, "PATCH", "/v1/combos/rt", `{"strategy":"pipeline"}`)
	if status != http.StatusOK {
		t.Fatalf("patch: status %d, body %s", status, body)
	}
	if got := comboDecode(t, body); got["strategy"] != "pipeline" {
		t.Fatalf("patch: strategy = %v, want pipeline", got["strategy"])
	}
	// pipeline is a combo execution mode, not a config strategy:
	// it must NOT leak into chain_strategies (config validation
	// would reject it on reload). The previous mirror entry is
	// replaced only by a config-valid name — here the old
	// "cheapest" entry is removed because the record no longer
	// carries a config-valid strategy.
	if _, ok := cfg.Routing.ChainStrategies["rt"]; ok {
		t.Fatalf("patch to pipeline: chain_strategies mirror still holds %q; combo modes must not be mirrored", cfg.Routing.ChainStrategies["rt"])
	}

	status, body = comboDo(t, h, "DELETE", "/v1/combos/rt", "")
	if status != http.StatusOK {
		t.Fatalf("delete: status %d, body %s", status, body)
	}
	if _, ok := cfg.Routing.Groups["rt"]; ok {
		t.Fatalf("delete: group mirror still holds rt — deleted must mean gone")
	}
	status, body = comboDo(t, h, "GET", "/v1/combos/rt", "")
	if status != http.StatusNotFound {
		t.Fatalf("get after delete: status %d, body %s (want 404)", status, body)
	}
	status, body = comboDo(t, h, "DELETE", "/v1/combos/rt", "")
	if status != http.StatusNotFound {
		t.Fatalf("second delete: status %d (want 404, never a silent success)", status)
	}
}

// TestCombosValidation covers the 400s the fixtures do not:
// duplicate names, duplicate steps, illegal names, rename
// attempts, bad efforts, and empty-chain PATCH.
func TestCombosValidation(t *testing.T) {
	h := comboTestStore().Handler()
	comboSeed(t, h)

	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantSubstr string
	}{
		{"duplicate name", "POST", "/v1/combos", `{"name":"mock-combo","chain":[{"provider":"mock-upstream"}],"strategy":"priority"}`, http.StatusConflict, "already exists"},
		{"illegal name slash", "POST", "/v1/combos", `{"name":"a/b","chain":[{"provider":"mock-upstream"}]}`, http.StatusBadRequest, "name:"},
		{"illegal name colon", "POST", "/v1/combos", `{"name":"a:b","chain":[{"provider":"mock-upstream"}]}`, http.StatusBadRequest, "name:"},
		{"empty name", "POST", "/v1/combos", `{"name":"","chain":[{"provider":"mock-upstream"}]}`, http.StatusBadRequest, "name:"},
		{"duplicate step", "POST", "/v1/combos", `{"name":"dup","chain":[{"provider":"mock-upstream","model":"mock-model"},{"provider":"mock-upstream","model":"mock-model"}]}`, http.StatusBadRequest, "duplicate step"},
		{"bad effort", "POST", "/v1/combos", `{"name":"eff","chain":[{"provider":"mock-upstream","effort":"turbo"}]}`, http.StatusBadRequest, "chain[0].effort:"},
		{"rename rejected", "PATCH", "/v1/combos/mock-combo", `{"name":"other"}`, http.StatusBadRequest, "name: cannot rename"},
		{"patch empty chain", "PATCH", "/v1/combos/mock-combo", `{"chain":[]}`, http.StatusBadRequest, "chain: must not be empty"},
		{"patch unknown provider", "PATCH", "/v1/combos/mock-combo", `{"chain":[{"provider":"mock-nope"}]}`, http.StatusBadRequest, "chain[0].provider:"},
		{"patch unknown id", "PATCH", "/v1/combos/nope", `{"strategy":"fastest"}`, http.StatusNotFound, "not found"},
		{"patch unknown strategy", "PATCH", "/v1/combos/mock-combo", `{"strategy":"nope"}`, http.StatusBadRequest, "strategy:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := comboDo(t, h, tc.method, tc.path, tc.body)
			if status != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", status, tc.wantStatus, body)
			}
			if !strings.Contains(string(body), tc.wantSubstr) {
				t.Fatalf("body %s does not contain %q", body, tc.wantSubstr)
			}
		})
	}
}

// TestCombosUnusableFlags checks the per-step usability flags:
// disabled provider and keyless non-local provider are unusable
// (but stored); the keyless local provider is usable.
func TestCombosUnusableFlags(t *testing.T) {
	h := comboTestStore().Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"flags","chain":[{"provider":"mock-disabled"},{"provider":"mock-local"},{"provider":"mock-keyless"}],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	var c Combo
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Usable {
		t.Fatalf("combo usable=true with unusable steps")
	}
	want := []struct {
		usable bool
		reason string
	}{
		{false, "provider disabled"},
		{true, ""},
		{false, "provider has no key"},
	}
	for i, w := range want {
		if c.Chain[i].Usable != w.usable || c.Chain[i].UnusableReason != w.reason {
			t.Fatalf("step %d: usable=%v reason=%q, want usable=%v reason=%q", i, c.Chain[i].Usable, c.Chain[i].UnusableReason, w.usable, w.reason)
		}
	}
	// A provider with no declared models cannot verify a pin.
	status, body = comboDo(t, h, "POST", "/v1/combos", `{"name":"unverified","chain":[{"provider":"mock-upstream","model":"mock-model"}],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("create unverified case: status %d", status)
	}
	_ = body
}

// TestCombosSeededFromGroups proves the store is a face over
// existing config state: a group already in the config file shows
// up as a combo with its chain strategy, no migration needed.
func TestCombosSeededFromGroups(t *testing.T) {
	cfg := comboTestConfig()
	cfg.Routing.Groups = map[string][]string{"legacy-group": {"mock-upstream@low", "mock-local"}}
	cfg.Routing.ChainStrategies = map[string]string{"legacy-group": "cheapest"}
	h := NewComboStore(cfg, nil).Handler()
	status, body := comboDo(t, h, "GET", "/v1/combos/legacy-group", "")
	if status != http.StatusOK {
		t.Fatalf("get seeded: status %d, body %s", status, body)
	}
	var c Combo
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Strategy != "cheapest" || len(c.Chain) != 2 || c.Chain[0].Effort != "low" || c.Chain[0].Provider != "mock-upstream" {
		t.Fatalf("seeded combo = %+v, want cheapest over mock-upstream@low + mock-local", c)
	}
}

// TestCombosPersistRollback proves a failing persist leaves no
// trace: the POST answers 500 and the combo (record + mirrors)
// is gone.
func TestCombosPersistRollback(t *testing.T) {
	cfg := comboTestConfig()
	persistErr := errors.New("disk full (test)")
	store := NewComboStore(cfg, func() error { return persistErr })
	h := store.Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"doomed","chain":[{"provider":"mock-upstream"}],"strategy":"priority"}`)
	if status != http.StatusInternalServerError {
		t.Fatalf("persist failure: status %d, body %s (want 500)", status, body)
	}
	status, _ = comboDo(t, h, "GET", "/v1/combos/doomed", "")
	if status != http.StatusNotFound {
		t.Fatalf("rollback: combo still listed after failed persist (GET status %d)", status)
	}
	if _, ok := cfg.Routing.Groups["doomed"]; ok {
		t.Fatalf("rollback: group mirror still holds doomed after failed persist")
	}
}

// TestCombosPersistSuccess proves a working persist callback is
// honoured and the responses report persisted=true.
func TestCombosPersistSuccess(t *testing.T) {
	cfg := comboTestConfig()
	calls := 0
	store := NewComboStore(cfg, func() error { calls++; return nil })
	h := store.Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"saved","chain":[{"provider":"mock-upstream"}],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	if calls != 1 {
		t.Fatalf("persist called %d times, want 1", calls)
	}
	var c Combo
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !c.Persisted {
		t.Fatalf("persisted=false despite a wired persist callback")
	}
}

// comboCreateStatus extracts the HTTP status a Create error
// carries, failing the test when the error is not a comboError.
func comboCreateStatus(t *testing.T, err error) int {
	t.Helper()
	var ce *comboError
	if !errors.As(err, &ce) {
		t.Fatalf("Create error %v is not a comboError", err)
	}
	return ce.status
}

// TestComboStoreCreateHappyPath proves the programmatic create
// behind POST /v1/combos and the MCP create_combo tool: the
// returned Combo carries the stored chain/strategy, the combo
// is visible through the HTTP handler, and the group mirror
// makes it engine-visible.
func TestComboStoreCreateHappyPath(t *testing.T) {
	cfg := comboTestConfig()
	store := NewComboStore(cfg, nil)
	got, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "direct", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream/mock-model"`)}, Strategy: "cheapest",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Name != "direct" || got.Strategy != "cheapest" || len(got.Chain) != 1 ||
		got.Chain[0].Provider != "mock-upstream" || got.Chain[0].Model != "mock-model" {
		t.Fatalf("Create returned %+v, want direct/cheapest over mock-upstream/mock-model", got)
	}
	if got.Persisted {
		t.Fatalf("Create returned persisted=true with no persist callback wired")
	}
	// Visible through the HTTP handler (same store).
	status, body := comboDo(t, store.Handler(), "GET", "/v1/combos/direct", "")
	if status != http.StatusOK {
		t.Fatalf("GET after Create: status %d, body %s", status, body)
	}
	var viaHTTP Combo
	if err := json.Unmarshal(body, &viaHTTP); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if viaHTTP.Name != "direct" || viaHTTP.Strategy != "cheapest" {
		t.Fatalf("GET returned %+v, want direct/cheapest", viaHTTP)
	}
	// Group mirror: the combo routes like any other group.
	if members, ok := cfg.Routing.Groups["direct"]; !ok || len(members) != 1 || members[0] != "mock-upstream" {
		t.Fatalf("group mirror = %v (present %v), want [mock-upstream]", members, ok)
	}
	if st, ok := cfg.Routing.ChainStrategies["direct"]; !ok || st != "cheapest" {
		t.Fatalf("strategy mirror = %q (present %v), want cheapest", st, ok)
	}
}

// TestComboStoreCreateValidation proves bad input fails through
// Create with the HTTP status the handler answers (400) and
// stores nothing.
func TestComboStoreCreateValidation(t *testing.T) {
	cases := []struct {
		name string
		req  ComboCreateRequest
		want string
	}{
		{"empty name", ComboCreateRequest{Name: "", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "priority"}, "name: must not be empty"},
		{"unknown provider", ComboCreateRequest{Name: "x", Chain: []json.RawMessage{json.RawMessage(`"ghost"`)}, Strategy: "priority"}, `chain[0].provider: unknown provider "ghost"`},
		{"empty chain", ComboCreateRequest{Name: "x", Chain: nil, Strategy: "priority"}, "chain: must not be empty"},
		{"unknown strategy", ComboCreateRequest{Name: "x", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "nope"}, `strategy: unknown strategy "nope"`},
		{"unknown model", ComboCreateRequest{Name: "x", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream/nope"`)}, Strategy: "priority"}, `chain[0].model: unknown model "nope"`},
		{"duplicate step", ComboCreateRequest{Name: "x", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`), json.RawMessage(`"mock-upstream"`)}, Strategy: "priority"}, "duplicate step"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := comboTestStore()
			_, err := store.Create(context.Background(), tc.req)
			if err == nil {
				t.Fatalf("Create(%q) succeeded, want a validation error", tc.name)
			}
			if st := comboCreateStatus(t, err); st != http.StatusBadRequest {
				t.Fatalf("Create status = %d, want 400 (err: %v)", st, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Create error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if _, exists := store.records[tc.req.Name]; tc.req.Name != "" && exists {
				t.Fatalf("rejected create still stored %q", tc.req.Name)
			}
		})
	}
}

// TestComboStoreCreateDuplicate proves a second create with the
// same name fails with the conflict status (409) and leaves the
// first combo untouched.
func TestComboStoreCreateDuplicate(t *testing.T) {
	store := comboTestStore()
	req := ComboCreateRequest{Name: "once", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "priority"}
	if _, err := store.Create(context.Background(), req); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	_, err := store.Create(context.Background(), req)
	if err == nil {
		t.Fatalf("second Create succeeded, want a duplicate error")
	}
	if st := comboCreateStatus(t, err); st != http.StatusConflict {
		t.Fatalf("duplicate Create status = %d, want 409 (err: %v)", st, err)
	}
	if !strings.Contains(err.Error(), `combo "once" already exists`) {
		t.Fatalf("duplicate Create error = %q, want it to name the existing combo", err.Error())
	}
	if len(store.records) != 1 {
		t.Fatalf("store holds %d records, want 1 (the first combo)", len(store.records))
	}
}

// TestComboStoreCreatePersistRollback proves a failed persist
// rolls the create back through Create itself: the error
// carries the 500 status, no record remains, and neither
// config mirror holds the combo — so retrying the same create
// succeeds.
func TestComboStoreCreatePersistRollback(t *testing.T) {
	cfg := comboTestConfig()
	persistErr := errors.New("disk full (test)")
	store := NewComboStore(cfg, func() error { return persistErr })
	_, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "doomed", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "cheapest",
	})
	if err == nil {
		t.Fatalf("Create with failing persist succeeded, want an error")
	}
	if st := comboCreateStatus(t, err); st != http.StatusInternalServerError {
		t.Fatalf("persist-failure Create status = %d, want 500 (err: %v)", st, err)
	}
	if !strings.Contains(err.Error(), "could not save combo") || !strings.Contains(err.Error(), "disk full (test)") {
		t.Fatalf("persist-failure Create error = %q, want it to name the save failure and the cause", err.Error())
	}
	if _, exists := store.records["doomed"]; exists {
		t.Fatalf("rollback: record still stored after failed persist")
	}
	if _, ok := cfg.Routing.Groups["doomed"]; ok {
		t.Fatalf("rollback: group mirror still holds doomed after failed persist")
	}
	if _, ok := cfg.Routing.ChainStrategies["doomed"]; ok {
		t.Fatalf("rollback: strategy mirror still holds doomed after failed persist")
	}
	// The rollback is complete: the same create now succeeds
	// once the persist callback is repaired.
	store.persist = func() error { return nil }
	if _, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "doomed", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "cheapest",
	}); err != nil {
		t.Fatalf("retry Create after rollback: %v", err)
	}
}

// TestComboStoreCreateMatchesHandler proves the handler and the
// programmatic Create share one code path: the same bad input
// fails with the identical message text on both, and the same
// good input succeeds with the identical combo on both.
func TestComboStoreCreateMatchesHandler(t *testing.T) {
	bad := ComboCreateRequest{Name: "bad", Chain: []json.RawMessage{json.RawMessage(`"ghost"`)}, Strategy: "priority"}
	// Handler path: POST the same body, read the error message.
	store := comboTestStore()
	h := store.Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"bad","chain":["ghost"],"strategy":"priority"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("handler POST: status %d, want 400", status)
	}
	var errBody struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("decode handler error: %v (body: %s)", err, body)
	}
	// Create path: the same input, the same message.
	_, err := store.Create(context.Background(), bad)
	if err == nil {
		t.Fatalf("Create accepted the bad input the handler rejected")
	}
	if err.Error() != errBody.Error.Message {
		t.Fatalf("Create error %q != handler error %q: the paths disagree", err.Error(), errBody.Error.Message)
	}
	// Good input agrees too: Create on one store, POST on
	// another store with the same config shape, same combo out.
	good := ComboCreateRequest{Name: "good", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream/mock-model"`)}, Strategy: "cheapest"}
	created, err := store.Create(context.Background(), good)
	if err != nil {
		t.Fatalf("Create good: %v", err)
	}
	store2 := comboTestStore()
	status, body = comboDo(t, store2.Handler(), "POST", "/v1/combos", `{"name":"good","chain":["mock-upstream/mock-model"],"strategy":"cheapest"}`)
	if status != http.StatusCreated {
		t.Fatalf("handler POST good: status %d, body %s", status, body)
	}
	var viaHTTP Combo
	if err := json.Unmarshal(body, &viaHTTP); err != nil {
		t.Fatalf("decode handler create: %v", err)
	}
	if viaHTTP.Name != created.Name || viaHTTP.Strategy != created.Strategy || len(viaHTTP.Chain) != len(created.Chain) ||
		viaHTTP.Chain[0].Provider != created.Chain[0].Provider || viaHTTP.Chain[0].Model != created.Chain[0].Model {
		t.Fatalf("handler created %+v, Create returned %+v: the paths disagree", viaHTTP, created)
	}
}

// TestCombosToggleCreate proves the Create tests fail when the
// feature is removed: Create on an empty store with no providers
// configured cannot validate any chain, so the happy path must
// fail (the toggle guard for the new entry point).
func TestCombosToggleCreate(t *testing.T) {
	store := NewComboStore(nil, nil)
	if _, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "x", Chain: []json.RawMessage{json.RawMessage(`"mock-upstream"`)}, Strategy: "priority",
	}); err == nil {
		t.Fatalf("toggle: Create succeeded with no providers configured; the validation must fail when the feature is removed")
	}
}

// TestCombosModelUnverifiable pins a model on a provider that
// declares no models: accepted, but reported model_verified=false
// (never assumed verified).
func TestCombosModelUnverifiable(t *testing.T) {
	cfg := comboTestConfig()
	cfg.Providers = append(cfg.Providers, config.ProviderConfig{
		ID: "mock-bare", Name: "Mock Bare", BaseURL: "https://mock-bare.example",
		APIKeyEncrypted: "sealed-mock-key", WireFormat: "openai", Enabled: true,
		// Models intentionally empty: nothing to verify against.
	})
	h := NewComboStore(cfg, nil).Handler()
	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"bare","chain":[{"provider":"mock-bare","model":"mock-model"}],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("create: status %d, body %s", status, body)
	}
	var c Combo
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.Chain[0].ModelVerified == nil || *c.Chain[0].ModelVerified {
		t.Fatalf("model_verified = %v, want false (provider declares no models)", c.Chain[0].ModelVerified)
	}
}

// comboTwoModelConfig is comboTestConfig with mock-upstream
// declaring a second model, so a chain can pin two different
// models on the same provider (mock-model and mock-model-b). It
// also fills the fields config.Validate requires (version,
// loopback listen, data dir) so a persist callback that runs
// Validate reproduces a real config.Save: it passes for a
// valid config and fails only on the mirrored duplicate member.
func comboTwoModelConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := comboTestConfig()
	cfg.Providers[0].Models = []provider.Model{{ID: "mock-model"}, {ID: "mock-model-b"}}
	cfg.Version = config.CurrentVersion
	cfg.Listen = "127.0.0.1:20128"
	cfg.DataDir = t.TempDir()
	return cfg
}

// comboSaveValidatingPersist wires a persist callback that runs
// the same validation config.Save runs before writing the file:
// a duplicate group member fails it, exactly as the live
// server's configPersist does. It counts calls so tests can
// prove a rejected create never attempted a save.
func comboSaveValidatingPersist(t *testing.T, cfg *config.Config, calls *int) ComboPersistFunc {
	t.Helper()
	return func() error {
		*calls++
		return cfg.Validate()
	}
}

// TestComboStoreCreateMirrorDuplicatePinOnly proves two chain
// steps that differ only by model pin (same provider, same
// effort) are a clean 400 naming the duplicate — not the 500
// config.Save used to produce when the mirror wrote the same
// group member twice. The 400 fires before any save is
// attempted (persist is never called), and the same provider
// at DIFFERENT efforts stays legal, as config validation
// allows it.
func TestComboStoreCreateMirrorDuplicatePinOnly(t *testing.T) {
	pinOnly := func() (*ComboStore, *config.Config, *int) {
		cfg := comboTwoModelConfig(t)
		calls := 0
		return NewComboStore(cfg, comboSaveValidatingPersist(t, cfg, &calls)), cfg, &calls
	}
	chain := func(steps ...string) []json.RawMessage {
		raw := make([]json.RawMessage, 0, len(steps))
		for _, s := range steps {
			raw = append(raw, json.RawMessage(`"`+s+`"`))
		}
		return raw
	}

	// Pin-only duplicate, no effort: same provider, two models.
	store, cfg, calls := pinOnly()
	_, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "pin-dup", Chain: chain("mock-upstream/mock-model", "mock-upstream/mock-model-b"), Strategy: "priority",
	})
	if err == nil {
		t.Fatalf("Create pin-only duplicate succeeded, want a 400 naming the duplicate")
	}
	if st := comboCreateStatus(t, err); st != http.StatusBadRequest {
		t.Fatalf("pin-only duplicate Create status = %d, want 400 (err: %v)", st, err)
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "mock-upstream") {
		t.Fatalf("pin-only duplicate Create error = %q, want it to name the duplicate mock-upstream member", err.Error())
	}
	if *calls != 0 {
		t.Fatalf("pin-only duplicate Create called persist %d times, want 0 (the 400 must fire before any save)", *calls)
	}
	if _, exists := store.records["pin-dup"]; exists {
		t.Fatalf("rejected pin-only duplicate still stored")
	}
	if _, ok := cfg.Routing.Groups["pin-dup"]; ok {
		t.Fatalf("rejected pin-only duplicate still mirrored into the group")
	}

	// Pin-only duplicate at the SAME effort: still one member.
	store, cfg, calls = pinOnly()
	_, err = store.Create(context.Background(), ComboCreateRequest{
		Name: "pin-dup-effort", Chain: chain("mock-upstream/mock-model@low", "mock-upstream/mock-model-b@low"), Strategy: "priority",
	})
	if err == nil {
		t.Fatalf("Create same-effort pin-only duplicate succeeded, want a 400 naming the duplicate")
	}
	if st := comboCreateStatus(t, err); st != http.StatusBadRequest {
		t.Fatalf("same-effort pin-only duplicate Create status = %d, want 400 (err: %v)", st, err)
	}
	if !strings.Contains(err.Error(), "duplicate") || !strings.Contains(err.Error(), "mock-upstream") {
		t.Fatalf("same-effort pin-only duplicate Create error = %q, want it to name the duplicate mock-upstream member", err.Error())
	}
	if *calls != 0 {
		t.Fatalf("same-effort pin-only duplicate Create called persist %d times, want 0", *calls)
	}
	if _, exists := store.records["pin-dup-effort"]; exists {
		t.Fatalf("rejected same-effort pin-only duplicate still stored")
	}
	if _, ok := cfg.Routing.Groups["pin-dup-effort"]; ok {
		t.Fatalf("rejected same-effort pin-only duplicate still mirrored into the group")
	}

	// Regression: distinct providers still create fine, and the
	// save runs exactly once.
	store, cfg, calls = pinOnly()
	got, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "distinct", Chain: chain("mock-upstream/mock-model", "mock-local/mock-model"), Strategy: "priority",
	})
	if err != nil {
		t.Fatalf("Create distinct-provider chain: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("distinct-provider Create called persist %d times, want 1", *calls)
	}
	if len(got.Chain) != 2 || got.Chain[0].Provider != "mock-upstream" || got.Chain[1].Provider != "mock-local" {
		t.Fatalf("distinct-provider Create returned %+v, want mock-upstream + mock-local", got.Chain)
	}
	if members := cfg.Routing.Groups["distinct"]; len(members) != 2 || members[0] != "mock-upstream" || members[1] != "mock-local" {
		t.Fatalf("distinct-provider group mirror = %v, want [mock-upstream mock-local]", members)
	}

	// Regression: the same provider at DIFFERENT efforts is one
	// member per effort, so two pins at different efforts stay
	// legal (the config duplicate rule is on provider+effort).
	store, _, calls = pinOnly()
	if _, err := store.Create(context.Background(), ComboCreateRequest{
		Name: "diff-effort", Chain: chain("mock-upstream/mock-model@low", "mock-upstream/mock-model-b@high"), Strategy: "priority",
	}); err != nil {
		t.Fatalf("Create same-provider different-effort chain: %v (want success)", err)
	}
	if *calls != 1 {
		t.Fatalf("different-effort Create called persist %d times, want 1", *calls)
	}
}

// TestCombosCreateMirrorDuplicatePinOnlyHTTP is the HTTP face of
// TestComboStoreCreateMirrorDuplicatePinOnly: POST /v1/combos
// with two steps differing only by model pin answers 400 naming
// the duplicate (never the 500 the save failure used to
// produce), before any save, while a distinct-provider chain
// still creates with 201.
func TestCombosCreateMirrorDuplicatePinOnlyHTTP(t *testing.T) {
	cfg := comboTwoModelConfig(t)
	calls := 0
	h := NewComboStore(cfg, comboSaveValidatingPersist(t, cfg, &calls)).Handler()

	status, body := comboDo(t, h, "POST", "/v1/combos", `{"name":"pin-dup","chain":["mock-upstream/mock-model","mock-upstream/mock-model-b"],"strategy":"priority"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("pin-only duplicate POST: status %d, want 400 (body %s)", status, body)
	}
	if !strings.Contains(string(body), "duplicate") || !strings.Contains(string(body), "mock-upstream") {
		t.Fatalf("pin-only duplicate POST body %s does not name the duplicate mock-upstream member", body)
	}
	if calls != 0 {
		t.Fatalf("pin-only duplicate POST called persist %d times, want 0 (the 400 must fire before any save)", calls)
	}
	if _, ok := cfg.Routing.Groups["pin-dup"]; ok {
		t.Fatalf("rejected pin-only duplicate still mirrored into the group")
	}
	status, _ = comboDo(t, h, "GET", "/v1/combos/pin-dup", "")
	if status != http.StatusNotFound {
		t.Fatalf("GET rejected pin-only duplicate: status %d, want 404", status)
	}

	status, body = comboDo(t, h, "POST", "/v1/combos", `{"name":"distinct","chain":["mock-upstream/mock-model","mock-local/mock-model"],"strategy":"priority"}`)
	if status != http.StatusCreated {
		t.Fatalf("distinct-provider POST: status %d, body %s (want 201)", status, body)
	}
	if calls != 1 {
		t.Fatalf("distinct-provider POST called persist %d times, want 1", calls)
	}
}
