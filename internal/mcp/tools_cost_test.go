package mcp

import (
	"strings"
	"testing"
	"time"
)

// newCostTestServer builds a server over a fakeSource with canned
// cost/quota answers: a priced cost report (all tokens priced,
// cost_known=true) and a quota report for key-1 (capped, $10
// limit, $4 spent).
func newCostTestServer() (*Server, *fakeSource) {
	f := &fakeSource{
		cost: CostReport{
			Days: 30, WindowStart: time.Now().AddDate(0, 0, -30), WindowEnd: time.Now(),
			Requests: 20, OK: 19, Errors: 1,
			PromptTokens: 2000, CompletionTokens: 1000, TotalTokens: 3000,
			CostUSD: 0.12, CostKnown: true,
			ByModel: []ModelCost{
				{Provider: "p1", Model: "coder-model", Requests: 20, PromptTokens: 2000, CompletionTokens: 1000, TotalTokens: 3000, CostUSD: 0.12, PriceKnown: true},
			},
		},
		quota: QuotaReport{
			KeyID: "key-1", HasBudget: true, LimitUSD: 10, SpentUSD: 4,
			RemainingUSD:   func() *float64 { v := 6.0; return &v }(),
			UnpricedPolicy: "fail_closed", ActiveWindow: "5h",
		},
	}
	return New(f, "0.1.0").WithClientIdentity(LocalOperator()), f
}

func TestM93CostReport(t *testing.T) {
	s, _ := newCostTestServer()
	sc := callTool(t, s, "cost_report", map[string]any{"days": 30})
	rep, _ := sc["cost_report"].(map[string]any)
	if rep["requests"] != float64(20) || rep["cost_usd"] != 0.12 || rep["cost_known"] != true {
		t.Errorf("cost_report = %v, want 20 requests / $0.12 / cost_known true", rep)
	}
	if rep["unpriced_tokens"] != nil && rep["unpriced_tokens"] != float64(0) {
		t.Errorf("unpriced_tokens = %v, want 0/absent for a fully priced report", rep["unpriced_tokens"])
	}
	// days out of range: -32602.
	if code := callToolErr(t, s, "cost_report", map[string]any{"days": 400}); code != -32602 {
		t.Errorf("days 400 code = %v, want -32602", code)
	}
	if code := callToolErr(t, s, "cost_report", map[string]any{"days": -1}); code != -32602 {
		t.Errorf("days -1 code = %v, want -32602", code)
	}
	// days must be an integer.
	if code := callToolErr(t, s, "cost_report", map[string]any{"days": 1.5}); code != -32602 {
		t.Errorf("days 1.5 code = %v, want -32602", code)
	}
}

func TestM93CostHonestyUnpricedIsUnknownNeverZero(t *testing.T) {
	// The cost-honesty test: a report whose usage is unpriced
	// must say UNKNOWN (cost_known=false, unpriced tokens and
	// models listed) — never $0.
	f := &fakeSource{
		cost: CostReport{
			Days: 7, Requests: 5, OK: 5,
			PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600,
			CostUSD: 0, CostKnown: false, UnpricedTokens: 600,
			UnpricedModels: []string{"p2/chat-model"},
			ByModel: []ModelCost{
				{Provider: "p2", Model: "chat-model", Requests: 5, PromptTokens: 500, CompletionTokens: 100, TotalTokens: 600, CostUSD: 0, PriceKnown: false, UnpricedTokens: 600},
			},
		},
	}
	s := New(f, "0.1.0").WithClientIdentity(LocalOperator())
	sc := callTool(t, s, "cost_report", map[string]any{"days": 7})
	rep, _ := sc["cost_report"].(map[string]any)
	if rep["cost_known"] != false {
		t.Fatalf("cost_known = %v for unpriced usage, want false (UNKNOWN, never $0)", rep["cost_known"])
	}
	if rep["cost_usd"] != float64(0) {
		t.Errorf("cost_usd = %v, want 0 as a lower bound with cost_known=false (not a true $0)", rep["cost_usd"])
	}
	if rep["unpriced_tokens"] != float64(600) {
		t.Errorf("unpriced_tokens = %v, want 600", rep["unpriced_tokens"])
	}
	models, _ := rep["unpriced_models"].([]any)
	if len(models) != 1 || models[0] != "p2/chat-model" {
		t.Errorf("unpriced_models = %v, want [p2/chat-model]", models)
	}
	byModel, _ := rep["by_model"].([]any)
	cell, _ := byModel[0].(map[string]any)
	if cell["price_known"] != false || cell["unpriced_tokens"] != float64(600) {
		t.Errorf("by_model cell = %v, want price_known=false / unpriced_tokens=600", cell)
	}
	// The text content carries the same payload (no silent $0 in
	// the human-readable half either).
	raw := call(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cost_report","arguments":{"days":7}}}`)
	result, _ := raw["result"].(map[string]any)
	content, _ := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"cost_known":false`) {
		t.Errorf("cost_report text does not carry cost_known=false: %s", text)
	}
}

func TestM93CheckQuota(t *testing.T) {
	s, _ := newCostTestServer()
	sc := callTool(t, s, "check_quota", map[string]any{"key_id": "key-1"})
	q, _ := sc["quota"].(map[string]any)
	if q["has_budget"] != true || q["limit_usd"] != float64(10) || q["spent_usd"] != float64(4) {
		t.Errorf("quota = %v, want capped $10 / $4 spent", q)
	}
	if q["remaining_usd"] != float64(6) {
		t.Errorf("remaining_usd = %v, want 6", q["remaining_usd"])
	}
	if q["unpriced_policy"] != "fail_closed" {
		t.Errorf("unpriced_policy = %v, want fail_closed", q["unpriced_policy"])
	}
	// Missing key_id: -32602.
	if code := callToolErr(t, s, "check_quota", map[string]any{}); code != -32602 {
		t.Errorf("missing key_id code = %v, want -32602", code)
	}
}

func TestM93SetBudgetGuard(t *testing.T) {
	s, f := newCostTestServer()
	sc := callTool(t, s, "set_budget_guard", map[string]any{"key_id": "key-2", "limit_usd": 25})
	if sc["guard_set"] != true {
		t.Errorf("guard_set = %v, want true", sc["guard_set"])
	}
	if f.guards["key-2"] != 25 {
		t.Errorf("fake guard for key-2 = %v, want 25", f.guards["key-2"])
	}
	// The reply re-reads the stored guard (not an echo).
	q, _ := sc["quota"].(map[string]any)
	if q["limit_usd"] != float64(25) || q["has_budget"] != true {
		t.Errorf("quota after set = %v, want limit 25 / has_budget true", q)
	}
	// Calendar caps + count_as_zero policy are accepted.
	if code := callToolErr(t, s, "set_budget_guard", map[string]any{"key_id": "key-3", "limit_usd": 5, "daily_limit_usd": 1, "unpriced_policy": "count_as_zero"}); code != 0 {
		t.Errorf("calendar-cap guard should succeed, got error code %v", code)
	}
	if f.guardPol["key-3"] != "count_as_zero" {
		t.Errorf("guard policy for key-3 = %q, want count_as_zero", f.guardPol["key-3"])
	}
}

func TestM93SetBudgetGuardInvalidInputs(t *testing.T) {
	s, f := newCostTestServer()
	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing key_id", map[string]any{"limit_usd": 5}},
		{"missing limit", map[string]any{"key_id": "k"}},
		{"negative limit", map[string]any{"key_id": "k", "limit_usd": -1}},
		{"unknown policy", map[string]any{"key_id": "k", "limit_usd": 5, "unpriced_policy": "ignore_it"}},
		{"negative daily cap", map[string]any{"key_id": "k", "limit_usd": 5, "daily_limit_usd": -2}},
		{"limit not a number", map[string]any{"key_id": "k", "limit_usd": "five"}},
	}
	for _, tc := range cases {
		if code := callToolErr(t, s, "set_budget_guard", tc.args); code != -32602 {
			t.Errorf("%s: code = %v, want -32602", tc.name, code)
		}
	}
	if len(f.guards) != 0 {
		t.Errorf("guards = %v after invalid sets, want none (nothing mutated)", f.guards)
	}
	// limit_usd 0 is valid (no spend allowed) — not an error.
	if code := callToolErr(t, s, "set_budget_guard", map[string]any{"key_id": "k0", "limit_usd": 0}); code != 0 {
		t.Errorf("limit 0 should succeed, got error code %v", code)
	}
}

func TestM93CostScopeDenial(t *testing.T) {
	// Read-cost scope reads but cannot set a guard; manage-cost
	// sets but cannot read. A routing-only key can do neither.
	s, _ := newCostTestServer()
	read := New(s.src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadCost}))
	if code := callToolErr(t, read, "cost_report", map[string]any{}); code != 0 {
		t.Errorf("read-cost cost_report should succeed, got error code %v", code)
	}
	if code := callToolErr(t, read, "check_quota", map[string]any{"key_id": "key-1"}); code != 0 {
		t.Errorf("read-cost check_quota should succeed, got error code %v", code)
	}
	if code := callToolErr(t, read, "set_budget_guard", map[string]any{"key_id": "k", "limit_usd": 1}); code != ScopeDeniedCode {
		t.Errorf("read-cost set_budget_guard code = %v, want -32002", code)
	}
	manage := New(s.src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeManageCost}))
	if code := callToolErr(t, manage, "cost_report", map[string]any{}); code != ScopeDeniedCode {
		t.Errorf("manage-cost cost_report code = %v, want -32002", code)
	}
	if code := callToolErr(t, manage, "check_quota", map[string]any{"key_id": "key-1"}); code != ScopeDeniedCode {
		t.Errorf("manage-cost check_quota code = %v, want -32002", code)
	}
	routingOnly := New(s.src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadRouting}))
	if code := callToolErr(t, routingOnly, "cost_report", map[string]any{}); code != ScopeDeniedCode {
		t.Errorf("routing-only cost_report code = %v, want -32002 (cost and routing scopes are disjoint families)", code)
	}
	// A denied set_budget_guard mutates nothing.
	if code := callToolErr(t, read, "set_budget_guard", map[string]any{"key_id": "ghost", "limit_usd": 1}); code != ScopeDeniedCode {
		t.Fatalf("denied set_budget_guard code = %v, want -32002", code)
	}
	if _, ok := s.src.(*fakeSource); !ok {
		t.Fatalf("test setup: server source is not the fake")
	}
	if _, ok := s.src.(*fakeSource).guards["ghost"]; ok {
		t.Errorf("denied set_budget_guard still stored a guard")
	}
}

func TestM93CostToolListIncludesCostTools(t *testing.T) {
	s, _ := newCostTestServer()
	out := call(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
	result, _ := out["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		m, _ := tool.(map[string]any)
		names[m["name"].(string)] = true
	}
	for _, want := range []string{"cost_report", "check_quota", "set_budget_guard"} {
		if !names[want] {
			t.Errorf("tools/list is missing %s", want)
		}
	}
}

func TestM93LedgerDownCostTools(t *testing.T) {
	down := New(&fakeSource{ledgerDown: true}, "0.1.0").WithClientIdentity(LocalOperator())
	if code, msg := callToolErrMsg(t, down, "cost_report", map[string]any{}); code != -32602 || !strings.Contains(msg, "unavailable:") {
		t.Errorf("ledger-down cost_report code/msg = %v / %q, want -32602 with 'unavailable:'", code, msg)
	}
	if code, msg := callToolErrMsg(t, down, "check_quota", map[string]any{"key_id": "k"}); code != -32602 || !strings.Contains(msg, "unavailable:") {
		t.Errorf("ledger-down check_quota code/msg = %v / %q, want -32602 with 'unavailable:'", code, msg)
	}
}

func TestM93CostKeyIDForcedToCaller(t *testing.T) {
	// M9.2 N3: a gateway-key caller may only act on its own key.
	// The rule is REJECT, not silent remap: a supplied key_id
	// naming a different key is refused with -32602, so the
	// operation can never apply to another key's quota/budget;
	// an omitted key_id defaults to the caller's own id. The
	// local operator keeps naming any key (same trust as the
	// router process). The set_budget_guard assertions are the
	// sharp end: the fake records which key a guard landed on,
	// so a cross-key write would show up in f.guards.
	_, f := newCostTestServer()
	read := New(f, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeReadCost}))
	manage := New(f, "0.1.0").WithClientIdentity(GatewayKeyClient("key-1", []string{ScopeManageCost}))

	// Reads for another key: rejected, naming the mismatch.
	if code, msg := callToolErrMsg(t, read, "cost_report", map[string]any{"key_id": "key-2"}); code != -32602 || !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("cost_report other-key code/msg = %v / %q, want -32602 naming the caller mismatch", code, msg)
	}
	if code, msg := callToolErrMsg(t, read, "check_quota", map[string]any{"key_id": "key-2"}); code != -32602 || !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("check_quota other-key code/msg = %v / %q, want -32602 naming the caller mismatch", code, msg)
	}
	// A write for another key: rejected AND nothing stored.
	if code := callToolErr(t, manage, "set_budget_guard", map[string]any{"key_id": "key-2", "limit_usd": 99}); code != -32602 {
		t.Errorf("set_budget_guard other-key code = %v, want -32602", code)
	}
	if _, ok := f.guards["key-2"]; ok {
		t.Errorf("rejected set_budget_guard still stored a guard for key-2")
	}
	// Own key, named or omitted: accepted, and the write lands
	// on the caller's own key.
	if code := callToolErr(t, read, "check_quota", map[string]any{"key_id": "key-1"}); code != 0 {
		t.Errorf("check_quota own-key should succeed, got error code %v", code)
	}
	if code := callToolErr(t, read, "check_quota", map[string]any{}); code != 0 {
		t.Errorf("check_quota with omitted key_id should default to the caller's own key, got error code %v", code)
	}
	if code := callToolErr(t, read, "cost_report", map[string]any{}); code != 0 {
		t.Errorf("cost_report with omitted key_id should default to the caller's own key, got error code %v", code)
	}
	if code := callToolErr(t, manage, "set_budget_guard", map[string]any{"limit_usd": 7}); code != 0 {
		t.Errorf("set_budget_guard with omitted key_id should default to the caller's own key, got error code %v", code)
	}
	if f.guards["key-1"] != 7 {
		t.Errorf("guard for caller key-1 = %v, want 7 (omitted key_id must land on the caller's own key)", f.guards["key-1"])
	}
	if code := callToolErr(t, manage, "set_budget_guard", map[string]any{"key_id": "key-1", "limit_usd": 9}); code != 0 {
		t.Errorf("set_budget_guard own-key should succeed, got error code %v", code)
	}
	if f.guards["key-1"] != 9 {
		t.Errorf("guard for caller key-1 = %v, want 9", f.guards["key-1"])
	}
	// Operator control: the local operator may still name any key.
	op, _ := newCostTestServer()
	if code := callToolErr(t, op, "set_budget_guard", map[string]any{"key_id": "key-9", "limit_usd": 3}); code != 0 {
		t.Errorf("operator set_budget_guard for key-9 should succeed, got error code %v", code)
	}
}
