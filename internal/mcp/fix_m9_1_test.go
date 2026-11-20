package mcp

// M9.1 SECURITY REVIEW fix tests (B1 + N1/N2/N3/N7/N9, 2026-11-19).
// Each test here pins one review finding so a future change that
// reintroduces it fails loudly instead of silently.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// TestNewDefaultsToAnonymousDenyAll is the B1 regression test: a
// bare New() server must deny every tool with -32002 — fail-closed,
// never the local operator's full grant. If New() ever defaults
// back to LocalOperator, this test fails (verified by toggle: see
// docs/LOG.md M9.1 fix entry). The stdio path opting into
// LocalOperator explicitly is proven separately by
// TestRunMCPToolsCallLiveData in cmd/codvian-router, which drives
// the production runMCPTo construction and asserts all three tools
// return live data.
func TestNewDefaultsToAnonymousDenyAll(t *testing.T) {
	log, _ := tempAuditLog(t)
	srv, src := newCountingServer() // bare New(), no WithClientIdentity
	srv.WithAuditLog(log)
	for _, tool := range []string{"router_status", "provider_list", "model_list"} {
		code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+tool+`","arguments":{}}}`)
		if code != float64(ScopeDeniedCode) {
			t.Errorf("bare New() server: %s code = %v, want -32002 (deny-all default)", tool, code)
		}
	}
	if src.statusCalls != 0 || src.providerCalls != 0 || src.modelCalls != 0 {
		t.Errorf("bare New() calls reached dispatch: %d/%d/%d, want 0", src.statusCalls, src.providerCalls, src.modelCalls)
	}
	// The denials are audited as the anonymous caller, not as the
	// local operator — the log must show who was actually denied.
	entries, err := log.List(0)
	if err != nil || len(entries) != 3 {
		t.Fatalf("audit entries = %d, err %v, want 3 anonymous denials", len(entries), err)
	}
	for i, e := range entries {
		if e.CallerKind != KindAnonymous || e.CallerID != "anonymous" {
			t.Errorf("entry %d caller = %s/%s, want anonymous/anonymous", i, e.CallerKind, e.CallerID)
		}
		if e.Decision != AuditDeny || e.ErrorCode != ScopeDeniedCode {
			t.Errorf("entry %d = %+v, want deny/-32002", i, e)
		}
	}
}

// TestMalformedParamsToolsCallIsAudited (N1): a tools/call whose
// params fail to unmarshal must be audited as a -32602 denial, not
// escape the log. The tool name is recorded when parseable,
// "unknown" when it is not.
func TestMalformedParamsToolsCallIsAudited(t *testing.T) {
	log, _ := tempAuditLog(t)
	srv, _ := newCountingServer()
	srv.WithAuditLog(log)
	srv.WithClientIdentity(LocalOperator())

	// Case 1: the name itself is unparseable (a number, not a
	// string) — the entry records tool "unknown".
	code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":42,"arguments":{}}}`)
	if code != -32602 {
		t.Errorf("malformed-params (bad name) code = %v, want -32602", code)
	}
	// Case 2: the name parses but the arguments do not (a string,
	// not an object) — the entry records the real tool name.
	code, _ = callErrCode(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"router_status","arguments":"nope"}}`)
	if code != -32602 {
		t.Errorf("malformed-params (bad arguments) code = %v, want -32602", code)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit entries = %d, err %v, want 2 malformed-params denials", len(entries), err)
	}
	// List is newest-first: reverse to call order.
	byCall := []AuditEntry{entries[1], entries[0]}
	if byCall[0].Tool != "unknown" || byCall[0].Decision != AuditDeny || byCall[0].ErrorCode != -32602 {
		t.Errorf("entry 0 = %+v, want deny/-32602 tool unknown", byCall[0])
	}
	if byCall[1].Tool != "router_status" || byCall[1].Decision != AuditDeny || byCall[1].ErrorCode != -32602 {
		t.Errorf("entry 1 = %+v, want deny/-32602 tool router_status", byCall[1])
	}
	for i, e := range byCall {
		if e.InputHash == "" {
			t.Errorf("entry %d has no input hash", i)
		}
		if e.OutputSummary != "" {
			t.Errorf("entry %d carries an output summary, want empty", i)
		}
	}
}

// TestAuditOpenTightensLoosePermissions (N2): opening a log over a
// pre-existing loose directory (0755) or file (0644) tightens both
// to 0700/0600 — creation-time modes alone do not fix paths that
// already exist. Existing entries survive the tighten (append,
// never truncate).
func TestAuditOpenTightensLoosePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mcp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod dir loose: %v", err)
	}
	path := filepath.Join(dir, "audit.jsonl")
	seed := `{"timestamp":"2026-11-19T00:00:00Z","tool":"router_status","caller_kind":"local-operator","caller_id":"local-operator","decision":"allow","input_hash":"abc"}` + "\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed loose file: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod file loose: %v", err)
	}
	log, err := OpenAuditLog(path)
	if err != nil {
		t.Fatalf("OpenAuditLog over loose paths: %v", err)
	}
	// Windows has no Unix permission bits (dirs report 0777, files
	// 0666), so the exact modes are only asserted off Windows. The
	// production code still calls Chmod (see OpenAuditLog); Windows
	// just cannot represent 0700/0600.
	if st, err := os.Stat(dir); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o700) {
		t.Errorf("dir perms = %v, err %v, want 0700", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("file perms = %v, err %v, want 0600", st.Mode().Perm(), err)
	}
	// The seeded entry survived, and appending still works with
	// the tightened permissions.
	if n, err := log.Count(); err != nil || n != 1 {
		t.Fatalf("Count after open = %d, err %v, want 1 (seed preserved)", n, err)
	}
	if err := log.Record(AuditEntry{Tool: "model_list", CallerKind: KindLocalOperator, CallerID: "local-operator", Decision: AuditAllow, InputHash: HashArguments(nil)}); err != nil {
		t.Fatalf("Record after tighten: %v", err)
	}
	if st, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && st.Mode().Perm() != 0o600) {
		t.Errorf("file perms after Record = %v, err %v, want 0600", st.Mode().Perm(), err)
	}
	if n, err := log.Count(); err != nil || n != 2 {
		t.Errorf("Count after Record = %d, err %v, want 2", n, err)
	}
}

// TestWithClientIdentityCopiesScopes (N3): mutating the caller's
// scopes slice after WithClientIdentity must not widen (or change)
// the server's stored grant.
func TestWithClientIdentityCopiesScopes(t *testing.T) {
	scopes := []string{ScopeReadProviders}
	srv, src := newCountingServer()
	srv.WithClientIdentity(ClientIdentity{Kind: KindGatewayKey, ID: "key-5", Scopes: scopes})
	scopes[0] = ScopeDestructive // widen the caller's slice after the call
	// provider_list must still work (the copied grant), and
	// router_status must still be denied (destructive alone
	// covers no read scope).
	out := call(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"provider_list","arguments":{}}}`)
	if _, ok := out["result"]; !ok {
		t.Fatalf("provider_list denied after source-slice mutation: %v", out)
	}
	if src.providerCalls != 1 {
		t.Errorf("providerCalls = %d, want 1", src.providerCalls)
	}
	code, _ := callErrCode(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"router_status","arguments":{}}}`)
	if code != float64(ScopeDeniedCode) {
		t.Errorf("router_status code = %v, want -32002 after source-slice mutation", code)
	}
	if src.statusCalls != 0 {
		t.Errorf("mutated grant reached dispatch: statusCalls = %d, want 0", src.statusCalls)
	}
}

// TestAuditConcurrentRecordNoInterleave (N7a): parallel Record
// calls must produce exactly N lines, every one valid JSON — no
// interleaved or corrupt lines, no lost entries.
func TestAuditConcurrentRecordNoInterleave(t *testing.T) {
	log, path := tempAuditLog(t)
	const goroutines = 20
	const perGoroutine = 25
	const want = goroutines * perGoroutine
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := log.Record(AuditEntry{
					Tool: "router_status", CallerKind: KindGatewayKey, CallerID: "key-conc",
					Decision: AuditAllow, InputHash: HashArguments(map[string]any{"g": g, "i": i}),
					OutputSummary: "ok",
				}); err != nil {
					t.Errorf("Record g%d i%d: %v", g, i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if err := log.LastError(); err != nil {
		t.Fatalf("LastError after concurrent records: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != want {
		t.Fatalf("raw log lines = %d, want exactly %d", len(lines), want)
	}
	for i, line := range lines {
		var e AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved/corrupt): %v: %q", i, err, line)
		}
		if e.Tool != "router_status" || e.CallerID != "key-conc" {
			t.Fatalf("line %d parsed as the wrong entry: %+v", i, e)
		}
	}
	if n, err := log.Count(); err != nil || n != want {
		t.Fatalf("Count = %d, err %v, want %d", n, err, want)
	}
	entries, err := log.List(0)
	if err != nil || len(entries) != want {
		t.Fatalf("List = %d entries, err %v, want %d", len(entries), err, want)
	}
}

// TestRegisterToolScopesPanicsOnReRegistration (N9): registering
// scopes for a tool that already has them panics — a silent
// replace would be a quiet privilege change.
func TestRegisterToolScopesPanicsOnReRegistration(t *testing.T) {
	RegisterToolScopes("test_m91_rereg_tool", ScopeReadStatus)
	mustPanic := func(name string, fn func()) {
		t.Helper()
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("RegisterToolScopes %s did not panic", name)
			}
		}()
		fn()
	}
	// A second registration with different scopes panics.
	mustPanic("second registration", func() {
		RegisterToolScopes("test_m91_rereg_tool", ScopeReadProviders)
	})
	// So does re-registering a built-in tool.
	mustPanic("built-in router_status", func() {
		RegisterToolScopes("router_status", ScopeReadProviders)
	})
	// And the original assignment is unchanged by the panicked
	// attempts.
	got, known := RequiredScopes("test_m91_rereg_tool")
	if !known || len(got) != 1 || got[0] != ScopeReadStatus {
		t.Errorf("scopes after panicked re-register = %v, known=%v, want [mcp:read:status]", got, known)
	}
}
