// Integration-B item A — A2A skills dispatch + WithCaller +
// prune-loop start (internal/server/a2a.go): end-to-end through
// the real /a2a handler. Every test here must FAIL on the
// pre-change code (verified by restoring the pre-change a2a.go /
// server.go and re-running — see the negative-control note in the
// parent report): skills/list and skills/invoke fall to -32601
// without the dispatch, the prune loop never starts without the
// a2aService wiring, and the cap branch is unreachable without
// the OpenWithLimits store.
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
	"time"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/config"
)

// a2aSkillsBody builds one skills/invoke JSON-RPC request body.
func a2aSkillsBody(skill, args string) string {
	if args == "" {
		args = "{}"
	}
	return a2aSendBody("skills/invoke", `{"skill":`+quoteJSON(skill)+`,"args":`+args+`}`)
}

func quoteJSON(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// a2aResultSkills extracts result.skills from a successful envelope.
func a2aResultSkills(t *testing.T, out map[string]any) []any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("envelope has no result: %v", out)
	}
	skills, ok := res["skills"].([]any)
	if !ok {
		t.Fatalf("result has no skills list: %v", res)
	}
	return skills
}

// TestA2ASkillsListReturnsTwelveDescriptors: skills/list returns the
// 6 skill descriptors from the live registry, in fixed order —
// the same list the agent card reports, never a hardcoded copy.
func TestA2ASkillsListReturnsTwelveDescriptors(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSendBody("skills/list", `{}`), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("skills/list: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("skills/list: unexpected error %d: %v", code, out)
	}
	got := a2aResultSkills(t, out)
	want := []string{"smart-routing", "quota-management", "provider-discovery", "cost-analysis", "health-report", "list-capabilities", "budget-forecasting", "cost-estimation", "route-preview", "compression-analytics", "eval-management", "org-reporting"}
	if len(got) != len(want) {
		t.Fatalf("skills/list: %d skills, want %d: %v", len(got), len(want), got)
	}
	for i, id := range want {
		d, ok := got[i].(map[string]any)
		if !ok {
			t.Fatalf("skills/list: skill %d is not an object: %#v", i, got[i])
		}
		if d["id"] != id {
			t.Errorf("skills/list: skill %d id = %v, want %q", i, d["id"], id)
		}
		if d["name"] == "" || d["description"] == "" {
			t.Errorf("skills/list: skill %q is missing name/description: %v", id, d)
		}
	}
}

// TestA2ASkillsInvokeUnknownSkillIs32602: invoking a skill that
// does not exist answers -32602 naming the skill — never -32601
// (the method exists) and never -32603 (nothing internal failed).
func TestA2ASkillsInvokeUnknownSkillIs32602(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSkillsBody("not-a-skill", `{}`), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("skills/invoke unknown: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -32602 {
		t.Fatalf("skills/invoke unknown: error code %d, want -32602: %v", code, out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, "not-a-skill") {
		t.Errorf("skills/invoke unknown: error message %q, want it to name the skill", msg)
	}
}

// TestA2ASkillsInvokeRequiresSkillName: skills/invoke with no
// skill (and no name alias) is -32602, not a dispatch panic or
// an invoke of the empty skill.
func TestA2ASkillsInvokeRequiresSkillName(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSendBody("skills/invoke", `{"args":{}}`), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("skills/invoke no skill: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -32602 {
		t.Fatalf("skills/invoke no skill: error code %d, want -32602: %v", code, out)
	}
}

// newA2ASkillsServer builds a server with a real issued key, a
// real ledger and a price book (the admission rig), plus a second
// issued key whose id the cross-key test can name. The first key
// is the caller; the second key is the foreign key.
func newA2ASkillsServer(t *testing.T) (*Server, *a2aAdmissionSetup, string) {
	t.Helper()
	s := newA2AAdmissionSetup(t)
	key2, _, err := config.IssueKey("second", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cfg.AddGatewayKey(key2); err != nil {
		t.Fatal(err)
	}
	return s.server(t), s, key2.ID
}

// TestA2ASkillsInvokeQuotaOwnKey: quota-management works for the
// authenticated key — the HTTP path carries a gateway-key caller,
// so resolveSkillKeyID settles on the caller's own id and the
// live ledger answers.
func TestA2ASkillsInvokeQuotaOwnKey(t *testing.T) {
	srv, s, _ := newA2ASkillsServer(t)
	status, out := a2aPost(t, srv, a2aSkillsBody("quota-management", `{}`), s.raw)
	if status != http.StatusOK {
		t.Fatalf("quota-management own key: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("quota-management own key: unexpected error %d (%s): %v", code, a2aErrMessage(t, out), out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("quota-management own key: envelope has no result: %v", out)
	}
	quota, ok := res["quota"].(map[string]any)
	if !ok {
		t.Fatalf("quota-management own key: result has no quota: %v", res)
	}
	if quota["key_id"] != s.id {
		t.Errorf("quota-management own key: quota key_id = %v, want the authenticated key %q", quota["key_id"], s.id)
	}
}

// TestA2ASkillsInvokeQuotaCrossKeyFailsClosed: quota-management
// with a key_id naming a DIFFERENT key fails closed. This is the
// proof the HTTP path carries a gateway-key caller and never a
// local-operator caller: a local operator may name any key, so
// only a gateway-key caller rejects the foreign id.
func TestA2ASkillsInvokeQuotaCrossKeyFailsClosed(t *testing.T) {
	srv, s, foreignID := newA2ASkillsServer(t)
	status, out := a2aPost(t, srv, a2aSkillsBody("quota-management", `{"key_id":`+quoteJSON(foreignID)+`}`), s.raw)
	if status != http.StatusOK {
		t.Fatalf("quota-management cross-key: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code == -1 {
		t.Fatalf("quota-management cross-key: no JSON-RPC error — the foreign key_id was accepted: %v", out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, foreignID) && !strings.Contains(msg, "not the authenticated caller") {
		t.Errorf("quota-management cross-key: error message %q, want it to name the rejected key", msg)
	}
}

// TestA2ASkillsInvokeListCapabilitiesOmitsRouterFactsForGatewayKey:
// the literal no-CallerLocalOperator-on-HTTP regression test
// (Integration-B nit N1). A gateway-key caller invoking
// skills/invoke list-capabilities over the real HTTP path must
// get a result that OMITS router facts: no "router" object —
// no version, listen address, provider/model counts,
// gateway_keys count, auto-router flag, or global strategy.
// If the HTTP path ever constructed a local-operator caller,
// skillListCapabilities would include exactly that object, so
// this test fails. Assertions are structural (key presence),
// not substring: the scope_note names the omitted facts in
// prose, so a raw-string search would false-positive on it.
func TestA2ASkillsInvokeListCapabilitiesOmitsRouterFactsForGatewayKey(t *testing.T) {
	srv, s, _ := newA2ASkillsServer(t)
	status, out := a2aPost(t, srv, a2aSkillsBody("list-capabilities", `{}`), s.raw)
	if status != http.StatusOK {
		t.Fatalf("list-capabilities gateway key: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("list-capabilities gateway key: unexpected error %d (%s): %v", code, a2aErrMessage(t, out), out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("list-capabilities gateway key: envelope has no result: %v", out)
	}
	if res["scope"] != "gateway-key" {
		t.Errorf("list-capabilities gateway key: scope = %v, want %q", res["scope"], "gateway-key")
	}
	if res["scoped_to_key"] != s.id {
		t.Errorf("list-capabilities gateway key: scoped_to_key = %v, want the authenticated key %q", res["scoped_to_key"], s.id)
	}
	if router, ok := res["router"]; ok {
		t.Errorf("list-capabilities gateway key: result carries a router object %v — router facts must be operator-only", router)
	}
	for _, key := range []string{"version", "listen", "providers", "models", "gateway_keys", "auto_router", "global_strategy"} {
		if _, ok := res[key]; ok {
			t.Errorf("list-capabilities gateway key: top-level key %q present — a router fact leaked", key)
		}
	}
	// Positive shape: the public registries are still listed.
	if _, ok := res["skills"]; !ok {
		t.Error("list-capabilities gateway key: result has no skills list")
	}
	if _, ok := res["strategies"]; !ok {
		t.Error("list-capabilities gateway key: result has no strategies list")
	}
	if _, ok := res["mcp_tools"]; !ok {
		t.Error("list-capabilities gateway key: result has no mcp_tools")
	}
}

// TestA2ASkillsInvokeNoCallerFailsClosed: InvokeSkill with no
// caller on the context fails closed (resolveSkillKeyID) instead
// of guessing whose key to read. This pins the fail-closed side
// of the contract the HTTP path relies on when it sets the
// caller with WithCaller.
func TestA2ASkillsInvokeNoCallerFailsClosed(t *testing.T) {
	srv, _, _ := newA2ASkillsServer(t)
	src := &serverMCPSource{srv: srv, st: mcpHTTPStateFor(srv)}
	_, err := a2a.InvokeSkill(context.Background(), src, "quota-management", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("InvokeSkill with no caller: no error, want a fail-closed caller-identity error")
	}
	if !strings.Contains(err.Error(), "caller identity required") {
		t.Errorf("InvokeSkill with no caller: error %q, want it to demand a caller identity", err.Error())
	}
}

// TestA2ASkillsInvokeProviderDiscoveryLive: provider-discovery
// runs against the live source over HTTP (no providers are
// configured on this server, so the honest answer is an empty
// list with count 0 — never an error, never invented rows).
func TestA2ASkillsInvokeProviderDiscoveryLive(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSkillsBody("provider-discovery", `{}`), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("provider-discovery: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("provider-discovery: unexpected error %d (%s): %v", code, a2aErrMessage(t, out), out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("provider-discovery: envelope has no result: %v", out)
	}
	if res["count"] != float64(0) {
		t.Errorf("provider-discovery: count = %v, want 0 (no providers configured)", res["count"])
	}
}

// TestA2ASkillsInvokeCostOwnKey: cost-analysis works for the
// authenticated key over HTTP (empty ledger, all-time window —
// the report comes from the live ledger, not a canned reply).
func TestA2ASkillsInvokeCostOwnKey(t *testing.T) {
	srv, s, _ := newA2ASkillsServer(t)
	status, out := a2aPost(t, srv, a2aSkillsBody("cost-analysis", `{"days":0}`), s.raw)
	if status != http.StatusOK {
		t.Fatalf("cost-analysis own key: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("cost-analysis own key: unexpected error %d (%s): %v", code, a2aErrMessage(t, out), out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("cost-analysis own key: envelope has no result: %v", out)
	}
	if _, ok := res["cost_report"]; !ok {
		t.Fatalf("cost-analysis own key: result has no cost_report: %v", res)
	}
}

// TestA2ASkillsInvokeHealthReportLive: health-report runs over
// HTTP against live state (no ledger is wired on this server, so
// the database health it reports is the honest unavailability
// shape from the source — the call itself must succeed).
func TestA2ASkillsInvokeHealthReportLive(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	status, out := a2aPost(t, srv, a2aSkillsBody("health-report", `{}`), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("health-report: status %d, want 200: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -1 {
		t.Fatalf("health-report: unexpected error %d (%s): %v", code, a2aErrMessage(t, out), out)
	}
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("health-report: envelope has no result: %v", out)
	}
	if _, ok := res["status"]; !ok {
		t.Fatalf("health-report: result has no status: %v", res)
	}
}

// TestA2APruneLoopStartsExactlyOnceFromService: the first
// a2aService() call starts exactly one prune loop (handle stored
// on the Server, running), and a second call — via the handler
// and directly — returns the same service and the same loop
// handle, never a second goroutine.
func TestA2APruneLoopStartsExactlyOnceFromService(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	t.Cleanup(srv.a2aStopPruneLoop)
	if srv.a2aPruneLoop != nil {
		t.Fatal("prune loop handle is non-nil before any /a2a call")
	}
	if _, out := a2aPost(t, srv, a2aSendBody("skills/list", `{}`), a2aKeyOne); a2aErrCode(t, out) != -1 {
		t.Fatalf("skills/list to start the service: %v", out)
	}
	loop := srv.a2aPruneLoop
	if loop == nil {
		t.Fatal("a2aService did not start a prune loop (handle is nil)")
	}
	if !loop.Running() {
		t.Error("prune loop handle exists but is not running")
	}
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("second a2aService: %v", err)
	}
	if svc != srv.a2aSvc {
		t.Error("second a2aService returned a different service")
	}
	if srv.a2aPruneLoop != loop {
		t.Error("second a2aService started a second prune loop (handle changed)")
	}
	if !loop.Running() {
		t.Error("prune loop stopped running after the second a2aService call")
	}
	if _, out := a2aPost(t, srv, a2aSendBody("skills/list", `{}`), a2aKeyOne); a2aErrCode(t, out) != -1 {
		t.Fatalf("second skills/list: %v", out)
	}
	if srv.a2aPruneLoop != loop {
		t.Error("second /a2a call started a second prune loop (handle changed)")
	}
}

// mustCompletedTaskForOwner creates a task for owner and drives it
// to Completed via the store (creation + working + completed).
// Unlike mustCompletedPruneTask (fixed prune-test owner), the
// owner here is the test's own key owner, so GetTask(owner, id)
// proves the task is really gone after pruning — not merely
// invisible to a different owner.
func mustCompletedTaskForOwner(t *testing.T, s *a2a.Store, owner string) *a2a.Task {
	t.Helper()
	task, err := s.Create(owner, json.RawMessage(`{"role":"user","content":"hi"}`))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Transition(owner, task.ID, a2a.StateWorking, nil, ""); err != nil {
		t.Fatalf("Transition to working: %v", err)
	}
	if _, err := s.Transition(owner, task.ID, a2a.StateCompleted, json.RawMessage(`{"content":"done"}`), ""); err != nil {
		t.Fatalf("Transition to completed: %v", err)
	}
	return task
}

// TestA2APruneLoopPrunesExpiredTaskFromService: the loop the
// service started prunes an expired terminal task at runtime.
// The server runs with a short test sweep interval; a completed
// task backdated 10 days (past the 7-day retention) must vanish
// from the store within a few sweeps.
func TestA2APruneLoopPrunesExpiredTaskFromService(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true)
	srv.a2aPruneOpts = &A2APruneOptions{
		RetentionDays:  DefaultA2ARetentionDays,
		MaxTasksPerKey: DefaultA2AMaxTasksPerKey,
		PruneEnabled:   boolPtr(true),
		Interval:       50 * time.Millisecond,
	}
	t.Cleanup(srv.a2aStopPruneLoop)
	svc, err := srv.a2aService()
	if err != nil {
		t.Fatalf("a2aService: %v", err)
	}
	if srv.a2aPruneLoop == nil || !srv.a2aPruneLoop.Running() {
		t.Fatal("prune loop did not start from a2aService")
	}
	owner := a2aOwnerForKey(t, srv, a2aKeyOne)
	task := mustCompletedTaskForOwner(t, svc.Store(), owner)
	backdateA2ATaskUpdatedAt(t, svc.Store(), task.ID, time.Now().UTC().AddDate(0, 0, -10))
	waitFor(t, 5*time.Second, "expired terminal task to be pruned by the service loop", func() bool {
		_, err := svc.GetTask(owner, task.ID)
		return errors.Is(err, a2a.ErrNotFound)
	})
}

// TestA2APruneLoopNotStartedWhileDisabled: with A2A off, no
// service opens and no prune loop starts — the disabled endpoint
// stays inert.
func TestA2APruneLoopNotStartedWhileDisabled(t *testing.T) {
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), false)
	if _, err := srv.a2aService(); !errors.Is(err, errA2ADisabled) {
		t.Fatalf("a2aService while disabled: err %v, want errA2ADisabled", err)
	}
	if srv.a2aPruneLoop != nil {
		t.Error("prune loop started while A2A is disabled")
	}
}

// TestA2AShutdownStopsPruneLoop: POST /admin/shutdown — the
// in-package shutdown path — stops the prune loop the service
// started (Integration-B nit N2). The stop is synchronous in
// handleShutdown, so by the time the handler returns the loop
// handle is dropped and the goroutine has exited; the configured
// shutdown hook still runs exactly as before (202 first, hook in
// its own goroutine).
func TestA2AShutdownStopsPruneLoop(t *testing.T) {
	hookDone := make(chan struct{})
	srv := newA2ATestServer(t, &fakeEngine{}, t.TempDir(), true,
		WithShutdownHook(func() { close(hookDone) }))
	// Start the service (and its prune loop) with a real /a2a call.
	if _, out := a2aPost(t, srv, a2aSendBody("skills/list", `{}`), a2aKeyOne); a2aErrCode(t, out) != -1 {
		t.Fatalf("skills/list to start the service: %v", out)
	}
	loop := srv.a2aPruneLoop
	if loop == nil || !loop.Running() {
		t.Fatal("prune loop did not start from the /a2a call")
	}
	// Loopback + valid key: 202, hook runs, loop stops.
	rec := doReqPeer(t, srv, http.MethodPost, "/admin/shutdown", "", a2aKeyOne, loopbackPeer)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /admin/shutdown: status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if srv.a2aPruneLoop != nil {
		t.Error("shutdown left a2aPruneLoop set (want nil after stop)")
	}
	if loop.Running() {
		t.Error("prune loop still running after POST /admin/shutdown")
	}
	select {
	case <-hookDone:
	case <-time.After(2 * time.Second):
		t.Error("shutdown hook did not run after POST /admin/shutdown")
	}
}

// TestA2ACreateTaskLimitIs32603WithCapMessage: with a per-key cap
// of 1, a second message/send while the first task is still
// non-terminal (its processor is blocked in the engine) answers
// -32603 carrying the cap message — the explicit
// ErrTaskLimitExceeded branch, not the generic create failure.
func TestA2ACreateTaskLimitIs32603WithCapMessage(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	srv.a2aPruneOpts = &A2APruneOptions{
		RetentionDays:  DefaultA2ARetentionDays,
		MaxTasksPerKey: 1,
		PruneEnabled:   boolPtr(true),
	}
	t.Cleanup(srv.a2aStopPruneLoop)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	post := func(body, key string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("POST /a2a response is not JSON (status %d): %v: %s", resp.StatusCode, err, raw)
		}
		return resp.StatusCode, out
	}
	// Start a stream; its processor blocks inside the engine, so
	// its task stays Working (non-terminal) and holds the cap.
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("message/stream: status %d: %s", resp.StatusCode, body)
	}
	lines := a2aSSELines(t, resp)
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat was never entered")
	}
	// Drain state events until the task is observed Working, so
	// the cap is provably held before the second create runs.
	timeout := time.After(10 * time.Second)
working:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the task reached Working")
			}
			if strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err == nil {
					if data["to_state"] == "working" {
						break working
					}
				}
			}
		case <-timeout:
			t.Fatal("task never reached Working within 10s")
		}
	}
	// The second create must hit the cap: -32603 naming it.
	status, out := post(a2aSendBody("message/send", a2aHelloParams), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("second send at cap: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -32603 {
		t.Fatalf("second send at cap: error code %d, want -32603: %v", code, out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, "task limit exceeded") || !strings.Contains(msg, "1 tasks") {
		t.Errorf("second send at cap: error message %q, want the cap message naming the 1-task limit", msg)
	}
	// Release the blocked processor so the stream (and the test
	// server) can finish cleanly.
	close(eng.release)
	select {
	case <-eng.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after release")
	}
}

// TestA2ACreateTaskLimitIs32603OnStreamWithCapMessage: the stream
// path's -32603 branch (Integration-B nit N3; a2a.go
// handleA2AStream). Mirrors
// TestA2ACreateTaskLimitIs32603WithCapMessage, but the second
// create is message/stream instead of message/send: with a
// per-key cap of 1, a second message/stream while the first
// task is still non-terminal (its processor is blocked in the
// engine) answers -32603 carrying the cap message — the
// explicit ErrTaskLimitExceeded branch, not the generic create
// failure. The capped stream answers plain JSON (not SSE):
// the error is written before any SSE headers are set.
func TestA2ACreateTaskLimitIs32603OnStreamWithCapMessage(t *testing.T) {
	eng := newA2ABlockingEngine()
	srv := newA2ATestServer(t, eng, t.TempDir(), true)
	srv.a2aPruneOpts = &A2APruneOptions{
		RetentionDays:  DefaultA2ARetentionDays,
		MaxTasksPerKey: 1,
		PruneEnabled:   boolPtr(true),
	}
	t.Cleanup(srv.a2aStopPruneLoop)
	httpSrv := httptest.NewServer(srv.Handler())
	defer httpSrv.Close()
	post := func(body, key string) (int, map[string]any) {
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("POST /a2a response is not JSON (status %d): %v: %s", resp.StatusCode, err, raw)
		}
		return resp.StatusCode, out
	}
	// Start a stream; its processor blocks inside the engine, so
	// its task stays Working (non-terminal) and holds the cap.
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/a2a", strings.NewReader(a2aSendBody("message/stream", a2aHelloParams)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+a2aKeyOne)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("message/stream: status %d: %s", resp.StatusCode, body)
	}
	lines := a2aSSELines(t, resp)
	select {
	case <-eng.started:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat was never entered")
	}
	// Drain state events until the task is observed Working, so
	// the cap is provably held before the second create runs.
	timeout := time.After(10 * time.Second)
working:
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream closed before the task reached Working")
			}
			if strings.HasPrefix(line, "data: ") {
				var data map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data); err == nil {
					if data["to_state"] == "working" {
						break working
					}
				}
			}
		case <-timeout:
			t.Fatal("task never reached Working within 10s")
		}
	}
	// The second create — a stream this time — must hit the cap:
	// -32603 naming it, as a plain JSON-RPC error (no SSE).
	status, out := post(a2aSendBody("message/stream", a2aHelloParams), a2aKeyOne)
	if status != http.StatusOK {
		t.Fatalf("second stream at cap: status %d, want 200 with a JSON-RPC error: %v", status, out)
	}
	if code := a2aErrCode(t, out); code != -32603 {
		t.Fatalf("second stream at cap: error code %d, want -32603: %v", code, out)
	}
	if msg := a2aErrMessage(t, out); !strings.Contains(msg, "task limit exceeded") || !strings.Contains(msg, "1 tasks") {
		t.Errorf("second stream at cap: error message %q, want the cap message naming the 1-task limit", msg)
	}
	// Release the blocked processor so the stream (and the test
	// server) can finish cleanly.
	close(eng.release)
	select {
	case <-eng.returned:
	case <-time.After(10 * time.Second):
		t.Fatal("engine Chat never returned after release")
	}
}
