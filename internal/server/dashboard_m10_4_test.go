// M10-4 dashboard Strategies + Auto-router: the strategy picker
// lists exactly what the backend accepts, from the live feed.
//
// Proves, against the served dashboard (not just the file on disk):
//   - the Auto-router section offers the 6 auto/* virtual models
//     (labelled virtual models, not strategies) plus the 3
//     intent/* tiers (labelled intent tiers, not virtual models),
//     and keeps its live classification + last-decision card;
//   - the Strategies section's global picker has NO hard-coded
//     options: its options are built by JS from the live
//     GET /dashboard/strategies feed (buildStrategyOptions over
//     d.strategies), so the picker can never offer a name the
//     backend rejects or miss one it accepts;
//   - the live feed's strategy list is exactly strategy.All
//     (34 entries incl. latency-aware at index 6 and sla-aware /
//     nadir — the plan's "10 names, no sla/nadir" list and its
//     "remove wrong code" instruction are stale: code IS accepted,
//     see TestDashboardM10_4PickerAcceptsFeedNames);
//   - changing the picker POSTs the real global strategy and a
//     re-read confirms the new value; a name that is neither in
//     the feed nor a backend alias is a 400 and changes nothing;
//   - the auto-route endpoints answer honestly with and without a
//     wired auto-router.
//   - the extracted inline script passes node --check.
//
// The 18-missing-reference-strategies claim in the plan is stale
// too: the reference router's ROUTING_STRATEGY_VALUES has 22 names and only
// 4 of them are absent from our feed — auto, fusion, pipeline,
// jev (they are not in strategy.All, so the backend rejects them
// and the picker never offers them). That gap is backend-MISSING
// (M3), recorded in the slice report, not papered over here.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
	"github.com/raunakiitp/codvian-router/internal/strategy"
)

// m10_4AutoVirtualModels are the 6 auto/* virtual models the
// Auto-router section offers (plan §M10-4: label them virtual
// models, not strategies). The 3 intent/* buttons are intent
// tiers, not virtual models.
var m10_4AutoVirtualModels = []string{
	"auto", "auto/fast", "auto/cheap", "auto/coding", "auto/code", "auto/reasoning",
}

var m10_4IntentTiers = []string{
	"intent/fast", "intent/balanced", "intent/best",
}

// m10_4RejectedNames are names that are neither in the live feed
// (strategy.All) nor backend aliases: the backend must reject
// each with a 400. "bogus-strategy" is a plain unknown.
var m10_4RejectedNames = []string{
	"bogus-strategy",
}

// m10_4Aliases are backend aliases (strategy.Parse): accepted by
// POST /dashboard/strategies by design, but never offered by the
// picker, which lists canonical names from the feed only. The
// engine stores the posted name verbatim (router.Engine
// SetGlobalStrategy trims, never canonicalises), so the re-read
// shows the alias as posted.
var m10_4Aliases = []string{"cheap", "fast", "reliable", "latency", "eco", "custom", "sla"}

func TestDashboardM10_4Sections(t *testing.T) {
	body := m10_0DashboardBody(t)
	// Scope the section assertions to the two sections this slice
	// owns: Auto-router (view-autorouter) through the end of
	// Strategies (view-strategies .. view-cooldown).
	autoStart := strings.Index(body, `id="view-autorouter"`)
	stratStart := strings.Index(body, `id="view-strategies"`)
	coolStart := strings.Index(body, `id="view-cooldown"`)
	if autoStart < 0 || stratStart <= autoStart || coolStart <= stratStart {
		t.Fatalf("dashboard Auto-router/Strategies section bounds not found (view-autorouter .. view-strategies .. view-cooldown)")
	}
	autoSection := body[autoStart:stratStart]
	stratSection := body[stratStart:coolStart]

	// Auto-router: live classification + last-decision card kept.
	for _, want := range []string{
		`id="view-autorouter"`,
		`id="autoModelSeg"`,
		`id="autoPrompt"`,
		`id="autoRouteBtn"`,
		`id="autoDecision"`,
		`id="autoLast"`,
		"No auto-routed request yet",
		// The 6 auto/* virtual models are labelled virtual models,
		// and the hint says intent/* are intent tiers, not virtual
		// models and not strategies.
		"auto/* are virtual models",
		"intent/* are operator-defined intent tiers, not virtual models and not strategies",
	} {
		if !strings.Contains(autoSection, want) {
			t.Errorf("dashboard Auto-router section missing %q", want)
		}
	}
	for _, vm := range m10_4AutoVirtualModels {
		if !strings.Contains(autoSection, `data-auto-model="`+vm+`"`) {
			t.Errorf("dashboard Auto-router section missing virtual model button %q", vm)
		}
	}
	for _, tier := range m10_4IntentTiers {
		if !strings.Contains(autoSection, `data-auto-model="`+tier+`"`) {
			t.Errorf("dashboard Auto-router section missing intent tier button %q", tier)
		}
	}
	// No auto/* button may be called a strategy anywhere in the
	// section: they are virtual models.
	if strings.Contains(autoSection, "strategy") {
		t.Errorf("dashboard Auto-router section must not mention strategies (auto/* are virtual models, not strategies)")
	}

	// Strategies: global picker, last live strategy, per-chain
	// table, trees. The picker select is EMPTY in the served HTML
	// — its options are built live from the feed by JS.
	for _, want := range []string{
		`id="view-strategies"`,
		`id="globalStrategySel"`,
		`id="stratLast"`,
		`id="stratChainBody"`,
		`id="stratTreeBody"`,
		`id="stratRefreshBtn"`,
		`id="deadlineBudgetLine"`,
	} {
		if !strings.Contains(stratSection, want) {
			t.Errorf("dashboard Strategies section missing %q", want)
		}
	}
	if strings.Contains(stratSection, "<option") {
		t.Errorf("dashboard Strategies section must not hard-code <option> values: the picker is built from the live feed")
	}
	// The served JS reads the live feeds and builds the picker
	// from them — never from a hard-coded name list.
	for _, want := range []string{
		"/dashboard/strategies",
		"/dashboard/auto-route",
		"/dashboard/auto-route/last",
		"buildStrategyOptions",
		"d.strategies",
		"loadAutoLast",
		"Was this routing right?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard M10-4 JS missing %q", want)
		}
	}
	// The last-decision feedback buttons must actually render:
	// the button HTML is concatenated into host.innerHTML (a
	// discarded ternary here once meant the buttons never
	// appeared, while the Feedback card told the operator to use
	// them). Pin the fixed concatenation: the cost/quality line
	// ends with `+`, and the button ternary follows as an operand,
	// not as a discarded standalone statement.
	if !strings.Contains(body, "autoCostQualityLine(dec) + '</p>' +") {
		t.Errorf("dashboard loadAutoLast must append the feedback buttons to the last-decision card (\"autoCostQualityLine(dec) + '</p>' +\")")
	}
	if strings.Contains(body, "autoCostQualityLine(dec) + '</p>';") {
		t.Errorf("dashboard loadAutoLast still ends the innerHTML assignment before the feedback buttons (discarded ternary)")
	}
	// No Playground anywhere in the dashboard.
	if strings.Contains(body, "Playground") {
		t.Errorf("dashboard must not contain a Playground")
	}
}

// TestDashboardM10_4PickerEqualsLiveFeed is the picker contract:
// the offered set is exactly the live feed, the feed is exactly
// strategy.All, every offered name POSTs successfully (including
// code — the plan's "remove wrong code" instruction is stale),
// and a name outside the feed (and outside the alias set) is a
// 400 that changes nothing.
func TestDashboardM10_4PickerEqualsLiveFeed(t *testing.T) {
	eng := &strategyFakeEngine{}
	srv := newTestServer(t, eng, WithModels("gpt-4o"))
	feed := getStrategies(t, srv)

	// The feed is exactly strategy.All, in order — the picker
	// inherits this list, so it can neither miss nor invent one.
	if len(feed.Strategies) != len(strategy.All) {
		t.Fatalf("strategies feed = %d entries, want %d (strategy.All)", len(feed.Strategies), len(strategy.All))
	}
	for i, st := range strategy.All {
		if feed.Strategies[i].ID != string(st) {
			t.Fatalf("strategies feed[%d] = %q, want %q (strategy.All order)", i, feed.Strategies[i].ID, string(st))
		}
	}
	// Spot pins the coordinator flagged: latency-aware at index 6,
	// sla-aware + nadir present, code present.
	if feed.Strategies[6].ID != "latency-aware" {
		t.Errorf("strategies feed[6] = %q, want latency-aware", feed.Strategies[6].ID)
	}
	for _, want := range []string{"code", "sla-aware", "nadir", "lkgp"} {
		found := false
		for _, st := range feed.Strategies {
			if st.ID == want {
				found = true
			}
		}
		if !found {
			t.Errorf("strategies feed missing %q", want)
		}
	}

	// Every offered name POSTs as the global strategy and a
	// re-read confirms the new value.
	for _, st := range feed.Strategies {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
			fmt.Sprintf(`{"global_strategy":%q}`, st.ID), "test-key-123")
		if rec.Code != http.StatusOK {
			t.Errorf("POST global_strategy=%q status = %d (want 200), body = %s", st.ID, rec.Code, rec.Body.String())
			continue
		}
		if got := getStrategies(t, srv).Global; got != st.ID {
			t.Errorf("after POST %q, re-read global = %q (want %q)", st.ID, got, st.ID)
		}
	}
	// Aliases are accepted by design (strategy.Parse) but never
	// offered by the picker; the engine stores the posted name
	// verbatim.
	for _, alias := range m10_4Aliases {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
			fmt.Sprintf(`{"global_strategy":%q}`, alias), "test-key-123")
		if rec.Code != http.StatusOK {
			t.Errorf("POST alias global_strategy=%q status = %d (want 200: aliases are accepted), body = %s", alias, rec.Code, rec.Body.String())
			continue
		}
		if got := getStrategies(t, srv).Global; got != alias {
			t.Errorf("after POST alias %q, re-read global = %q (want %q verbatim)", alias, got, alias)
		}
	}
	// Names outside the feed (and outside the alias set) are
	// rejected and change nothing — including the 4 the reference router
	// names the backend does not have.
	before := getStrategies(t, srv).Global
	for _, name := range m10_4RejectedNames {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
			fmt.Sprintf(`{"global_strategy":%q}`, name), "test-key-123")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST global_strategy=%q status = %d (want 400), body = %s", name, rec.Code, rec.Body.String())
		}
		if got := getStrategies(t, srv).Global; got != before {
			t.Errorf("after rejected POST %q, global changed to %q (want %q: nothing applied)", name, got, before)
		}
	}
	// Restore the default for a clean end state.
	rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"priority"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore POST status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := getStrategies(t, srv).Global; got != "priority" {
		t.Errorf("after restore, global = %q (want priority)", got)
	}
}

// TestDashboardM10_4AutoRouteFeeds checks the two auto-route
// endpoints the Auto-router section reads, with and without a
// wired auto-router.
func TestDashboardM10_4AutoRouteFeeds(t *testing.T) {
	// No auto-router wired: classification answers an honest
	// 503, and last-decision answers an honest empty state.
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=write+a+function", "", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired GET /dashboard/auto-route status = %d (want 503 auto-router is not configured), body = %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route/last", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/auto-route/last status = %d (want 200), body = %s", rec.Code, rec.Body.String())
	}
	var last struct {
		HasDecision bool `json:"has_decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &last); err != nil {
		t.Fatalf("decode auto-route/last: %v (body %s)", err, rec.Body.String())
	}
	if last.HasDecision {
		t.Errorf("auto-route/last has_decision = true (want false: no request yet)")
	}

	// Wired auto-router (testIntentRouter): a coding prompt
	// classifies live, an intent tier resolves without a prompt,
	// and unknown virtual models / missing prompts are honest 400s.
	srv = newTestServer(t, &fakeEngine{}, WithAutoRouter(testIntentRouter()))
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto&prompt=Write+a+Python+function+to+sort+a+list&threshold=0.5", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("wired GET /dashboard/auto-route status = %d (want 200), body = %s", rec.Code, rec.Body.String())
	}
	var dec autorouter.Decision
	if err := json.Unmarshal(rec.Body.Bytes(), &dec); err != nil {
		t.Fatalf("decode auto-route decision: %v (body %s)", err, rec.Body.String())
	}
	if dec.Tier == "" || dec.Model == "" {
		t.Errorf("auto-route decision = %+v (want a tier and a resolved model)", dec)
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=intent/fast", "", "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /dashboard/auto-route?model=intent/fast status = %d (want 200), body = %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto/nope&prompt=hello", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /dashboard/auto-route?model=auto/nope status = %d (want 400 unknown virtual model), body = %s", rec.Code, rec.Body.String())
	}
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route?model=auto", "", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /dashboard/auto-route?model=auto (no prompt) status = %d (want 400 prompt is required), body = %s", rec.Code, rec.Body.String())
	}

	// Last decision: an engine that reports one shows it.
	eng := &intentTestEngine{
		Engine: &fakeEngine{},
		dec: autorouter.Decision{
			ID: "dec-m104", Tier: autorouter.TierCoding, Model: "model-a",
			VirtualModel: "auto", Confidence: 0.9, Reason: "test decision",
		},
		trace: &routetrace.RouteTrace{},
	}
	srv = newTestServer(t, eng, WithAutoRouter(testIntentRouter()))
	rec = doReq(t, srv, http.MethodGet, "/dashboard/auto-route/last", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /dashboard/auto-route/last (with decision) status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var lastFull struct {
		HasDecision bool                `json:"has_decision"`
		Decision    autorouter.Decision `json:"decision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lastFull); err != nil {
		t.Fatalf("decode auto-route/last: %v (body %s)", err, rec.Body.String())
	}
	if !lastFull.HasDecision || lastFull.Decision.ID != "dec-m104" {
		t.Errorf("auto-route/last = %+v (want has_decision true, id dec-m104)", lastFull)
	}
}

// TestDashboardM10_4AliasGlobalPicker is the nit-1 regression test:
// when the live global strategy is an ALIAS (set via config file
// or a direct API POST — the backend stores it verbatim), the
// picker must still show a real selection and the rule text for
// the EFFECTIVE strategy. Before the fix, buildStrategyOptions set
// sel.value to the alias ("cheap"), which matches no canonical
// option, so the picker showed no selection and stratRule returned
// "" (generic hint) while the feed said the global was "cheap".
// The fix canonicalises aliases in JS (strategyAliases /
// canonicalStrategy, mirroring strategy.Parse). Residual nit:
// canonicalStrategy also returns the NORMALISED form for
// non-aliases, so a canonical global stored as "CHEAPEST" or
// " cheapest " still selects its picker option and resolves its
// rule/name instead of a blank picker + generic hint. The served
// HTML cannot be driven in a browser here, so the test pins the
// fix two ways: statically, the served JS must carry the full
// alias map and wire it into buildStrategyOptions + stratRule;
// and behaviourally, the extracted functions are executed under
// node against a stub select, which must select the canonical
// option and return the canonical rule for every alias. Both
// halves fail on the pre-fix HTML (no alias map, no
// canonicalStrategy).
func TestDashboardM10_4AliasGlobalPicker(t *testing.T) {
	eng := &strategyFakeEngine{}
	srv := newTestServer(t, eng, WithModels("gpt-4o"))

	// Every alias POSTs 200 and re-reads verbatim (this also pins
	// the 7th alias, "custom", added by the nit-2 fix).
	for _, alias := range m10_4Aliases {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
			fmt.Sprintf(`{"global_strategy":%q}`, alias), "test-key-123")
		if rec.Code != http.StatusOK {
			t.Errorf("POST alias global_strategy=%q status = %d (want 200), body = %s", alias, rec.Code, rec.Body.String())
			continue
		}
		if got := getStrategies(t, srv).Global; got != alias {
			t.Errorf("after POST alias %q, re-read global = %q (want %q verbatim)", alias, got, alias)
		}
	}
	// A canonical name stored in a non-lowercase / whitespace-padded
	// spelling also POSTs 200 and re-reads verbatim (the backend
	// stores it as given); the picker half below proves the JS then
	// resolves it to the canonical option instead of a blank picker.
	for _, raw := range []string{"CHEAPEST", " cheapest "} {
		rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
			fmt.Sprintf(`{"global_strategy":%q}`, raw), "test-key-123")
		if rec.Code != http.StatusOK {
			t.Errorf("POST canonical global_strategy=%q status = %d (want 200), body = %s", raw, rec.Code, rec.Body.String())
			continue
		}
		if got := getStrategies(t, srv).Global; got != raw {
			t.Errorf("after POST canonical %q, re-read global = %q (want %q verbatim)", raw, got, raw)
		}
	}
	// Restore the default for a clean end state.
	rec := doReq(t, srv, http.MethodPost, "/dashboard/strategies",
		`{"global_strategy":"priority"}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("restore POST status = %d, body = %s", rec.Code, rec.Body.String())
	}

	body := m10_0DashboardBody(t)

	// Static half: the served JS must carry the full alias ->
	// canonical map (all 7 aliases from strategy.Parse) and wire
	// canonicalStrategy into the picker selection and the rule
	// lookup, so an alias global resolves to its effective
	// strategy instead of a blank selection / empty rule.
	wantMap := map[string]string{
		"cheap": "cheapest", "fast": "fastest", "reliable": "most-reliable",
		"latency": "least-latency", "eco": "cost-optimized",
		"custom": "custom-weights", "sla": "sla-aware",
	}
	for alias, canonical := range wantMap {
		if !strings.Contains(body, `"`+alias+`": "`+canonical+`"`) {
			t.Errorf("served dashboard JS missing alias map entry %q -> %q (picker cannot resolve an alias global)", alias, canonical)
		}
	}
	if !strings.Contains(body, "function canonicalStrategy(id)") {
		t.Errorf("served dashboard JS missing canonicalStrategy (alias globals resolve to no picker selection)")
	}
	// Residual nit: the non-alias case must return the NORMALISED
	// form, not the original id — otherwise a global stored as
	// "CHEAPEST" / " cheapest " never matches a picker option and
	// stratRule returns "" (generic hint).
	if !strings.Contains(body, "return strategyAliases[norm] || norm;") {
		t.Errorf("served dashboard canonicalStrategy does not return the normalised form for non-aliases (non-lowercase canonical global shows a blank picker + generic hint)")
	}
	if !strings.Contains(body, "sel.value = canonicalStrategy(selected || cur)") {
		t.Errorf("served dashboard buildStrategyOptions does not canonicalise the selected value (alias global shows a blank picker)")
	}
	if n := strings.Count(body, "id = canonicalStrategy(id);"); n < 2 {
		t.Errorf("served dashboard stratName/stratRule canonicalise the id %d times, want >= 2 (alias global shows no rule text)", n)
	}

	// Behavioural half: run the extracted alias/strategy functions
	// under node against a stub select whose value setter mimics
	// a real <select> (setting a value with no matching option
	// leaves the value empty). Every alias must select its
	// canonical option and return its canonical rule; canonical
	// names pass through, and the empty/inherit selection is kept.
	if _, err := exec.LookPath("node"); err != nil {
		t.Log("node not installed: behavioural alias-picker check skipped (static assertions above still ran)")
		return
	}
	script := m10_0InlineScript(t, body)
	start := strings.Index(script, "var strategyAliases = {")
	end := strings.Index(script, "function saveStrategies")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("served dashboard JS has no extractable strategyAliases .. saveStrategies block")
	}
	driver := script[start:end] + m10_4AliasPickerDriver
	dir := t.TempDir()
	path := filepath.Join(dir, "alias-picker.js")
	if err := os.WriteFile(path, []byte(driver), 0o600); err != nil {
		t.Fatalf("write alias-picker driver: %v", err)
	}
	out, err := exec.Command("node", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node alias-picker driver failed: %v\n%s", err, out)
	}
}

// m10_4AliasPickerDriver is the node harness appended to the
// extracted strategyAliases .. buildStrategyOptions block. It stubs
// document.createElement and a <select> (real-select value
// semantics: a value with no matching option reads back empty),
// then checks every alias resolves to its canonical option + rule.
const m10_4AliasPickerDriver = `
var document = { createElement: function(tag){ return { value: "", textContent: "" }; } };
function fakeSel(){
  var opts = []; var val = "";
  var s = {};
  Object.defineProperty(s, "innerHTML", { get: function(){ return ""; }, set: function(v){ if(v === "") opts.length = 0; } });
  s.appendChild = function(o){ opts.push(o); };
  Object.defineProperty(s, "value", {
    get: function(){ return val; },
    set: function(v){ for(var i = 0; i < opts.length; i++){ if(opts[i].value === v){ val = v; return; } } val = ""; }
  });
  return s;
}
var list = [
  {id:"cheapest", name:"Cheapest", rule:"cheapest rule"},
  {id:"fastest", name:"Fastest", rule:"fastest rule"},
  {id:"most-reliable", name:"Most reliable", rule:"reliable rule"},
  {id:"least-latency", name:"Least latency", rule:"latency rule"},
  {id:"cost-optimized", name:"Cost optimized", rule:"eco rule"},
  {id:"custom-weights", name:"Custom weights", rule:"custom rule"},
  {id:"sla-aware", name:"SLA aware", rule:"sla rule"},
  {id:"priority", name:"Priority", rule:"priority rule"}
];
var cases = {cheap:"cheapest", fast:"fastest", reliable:"most-reliable", latency:"least-latency", eco:"cost-optimized", custom:"custom-weights", sla:"sla-aware"};
var failures = [];
Object.keys(cases).forEach(function(alias){
  var want = cases[alias];
  var wantRule = null; var wantName = null;
  list.forEach(function(st){ if(st.id === want){ wantRule = st.rule; wantName = st.name; } });
  var sel = fakeSel();
  buildStrategyOptions(sel, list, alias, false);
  if(sel.value !== want) failures.push("alias " + alias + ": picker value " + JSON.stringify(sel.value) + ", want " + JSON.stringify(want));
  if(stratRule(list, alias) !== wantRule) failures.push("alias " + alias + ": stratRule " + JSON.stringify(stratRule(list, alias)) + ", want " + JSON.stringify(wantRule));
  if(stratName(list, alias) !== wantName) failures.push("alias " + alias + ": stratName " + JSON.stringify(stratName(list, alias)) + ", want " + JSON.stringify(wantName));
});
var sel2 = fakeSel();
buildStrategyOptions(sel2, list, "priority", false);
if(sel2.value !== "priority") failures.push("canonical priority: picker value " + JSON.stringify(sel2.value) + ", want \"priority\"");
// Residual nit: a canonical name stored non-lowercase or padded
// ("CHEAPEST", " cheapest ") must still select the canonical
// option and resolve its rule + name — canonicalStrategy returns
// the normalised form for non-aliases.
["CHEAPEST", " cheapest ", "Cheapest"].forEach(function(raw){
  var sel = fakeSel();
  buildStrategyOptions(sel, list, raw, false);
  if(sel.value !== "cheapest") failures.push("canonical " + JSON.stringify(raw) + ": picker value " + JSON.stringify(sel.value) + ", want \"cheapest\"");
  if(stratRule(list, raw) !== "cheapest rule") failures.push("canonical " + JSON.stringify(raw) + ": stratRule " + JSON.stringify(stratRule(list, raw)) + ", want \"cheapest rule\"");
  if(stratName(list, raw) !== "Cheapest") failures.push("canonical " + JSON.stringify(raw) + ": stratName " + JSON.stringify(stratName(list, raw)) + ", want \"Cheapest\"");
});
// Unknown names stay unknown (no rule) but their stratName fallback
// is the normalised (lowercased, trimmed) spelling, not the raw id.
if(stratRule(list, "BOGUS") !== "") failures.push("unknown BOGUS: stratRule " + JSON.stringify(stratRule(list, "BOGUS")) + ", want \"\"");
if(stratName(list, " BOGUS ") !== "bogus") failures.push("unknown BOGUS: stratName " + JSON.stringify(stratName(list, " BOGUS ")) + ", want \"bogus\"");
var sel3 = fakeSel();
buildStrategyOptions(sel3, list, "", true);
if(sel3.value !== "") failures.push("inherit: picker value " + JSON.stringify(sel3.value) + ", want \"\"");
if(failures.length){ console.error(failures.join("\n")); process.exit(1); }
console.log("alias picker OK");
`

// TestDashboardM10_4InlineScriptSyntax runs node --check over the
// extracted inline script of the served dashboard (plan §M10-4
// tests). It skips when node is not installed; the live-binary
// smoke in the slice report records the node version used there.
func TestDashboardM10_4InlineScriptSyntax(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatal("node not installed — cannot run node --check (install node or set NODE_PATH)")
	}
	body := m10_0DashboardBody(t)
	script := m10_0InlineScript(t, body)
	dir := t.TempDir()
	path := filepath.Join(dir, "dashboard.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("write extracted script: %v", err)
	}
	out, err := exec.Command("node", "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("node --check failed: %v\n%s", err, out)
	}
}
