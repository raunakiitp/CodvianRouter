package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newOpsTestServer builds a server whose Source is a fake with a
// real tenant-scoped in-memory cache and a real file-backed memory
// store (temp dir), called as gateway key "key-a" holding the
// manage + destructive scopes the M9.4 tools need. Tests that
// need a narrower identity build their own server on the same
// Source via serverFor.
func newOpsTestServer(t *testing.T) (*Server, *fakeSource) {
	t.Helper()
	src := &fakeSource{
		providers: []ProviderInfo{{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}}},
		models:    []ModelInfo{{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"}},
		status:    StatusInfo{Version: "0.1.0", Listen: "127.0.0.1:20128", Providers: 1, EnabledProviders: 1, Models: 1, GatewayKeys: 1, AutoRouter: true, UptimeSeconds: -1},
		cache: map[string]map[string]string{
			"key-a": {"k1": "answer one", "k2": "answer two"},
			"key-b": {"k3": "other answer"},
		},
		mem: NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl")),
	}
	s := New(src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-a", []string{ScopeManageCache, ScopeManageMemory, ScopeManageOps, ScopeDestructive}))
	return s, src
}

// serverFor builds a server on src as the named key with exactly
// the given scopes — the denial tests' narrow identities.
func serverFor(src *fakeSource, keyID string, scopes []string) *Server {
	return New(src, "0.1.0").WithClientIdentity(GatewayKeyClient(keyID, scopes))
}

func callTool(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	msg, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(rawArgs)},
	})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	out := call(t, s, string(msg))
	if errBody, ok := out["error"].(map[string]any); ok {
		t.Fatalf("tools/call %s returned error %v (message %v)", name, errBody["code"], errBody["message"])
	}
	result, _ := out["result"].(map[string]any)
	sc, _ := result["structuredContent"].(map[string]any)
	if sc == nil {
		t.Fatalf("tools/call %s has no structuredContent: %v", name, out)
	}
	return sc
}

// callToolErr runs a tools/call and returns its JSON-RPC error
// code (0 when the call succeeded).
func callToolErr(t *testing.T, s *Server, name string, args map[string]any) float64 {
	t.Helper()
	rawArgs, _ := json.Marshal(args)
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(rawArgs)},
	})
	raw := s.Handle(context.Background(), msg)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if errBody, ok := out["error"].(map[string]any); ok {
		code, _ := errBody["code"].(float64)
		return code
	}
	return 0
}

func TestOpsToolsRegisteredWithScopes(t *testing.T) {
	want := map[string][]string{
		"cache_stats":   {ScopeManageCache},
		"cache_flush":   {ScopeManageCache, ScopeDestructive},
		"db_health":     {ScopeManageOps},
		"db_stats":      {ScopeManageOps},
		"memory_add":    {ScopeManageMemory},
		"memory_search": {ScopeManageMemory},
		"memory_clear":  {ScopeManageMemory, ScopeDestructive},
	}
	names := map[string]bool{}
	for _, tool := range Tools() {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has an empty description", tool.Name)
		}
	}
	for name, scopes := range want {
		if !names[name] {
			t.Errorf("tool %s is not registered", name)
			continue
		}
		got, known := RequiredScopes(name)
		if !known {
			t.Errorf("tool %s has no scope assignment", name)
			continue
		}
		if len(got) != len(scopes) {
			t.Errorf("tool %s scopes = %v, want %v", name, got, scopes)
			continue
		}
		for i := range scopes {
			if got[i] != scopes[i] {
				t.Errorf("tool %s scopes = %v, want %v", name, got, scopes)
			}
		}
	}
}

func TestOpsCacheStatsReturnsRealCounts(t *testing.T) {
	s, _ := newOpsTestServer(t)
	sc := callTool(t, s, "cache_stats", map[string]any{})
	exact, _ := sc["exact"].(map[string]any)
	if exact["entries"] != float64(3) {
		t.Errorf("cache_stats exact entries = %v, want 3", exact["entries"])
	}
	// "answer one" (10) + "answer two" (10) + "other answer" (12).
	if exact["bytes"] != float64(32) {
		t.Errorf("cache_stats exact bytes = %v, want 32", exact["bytes"])
	}
	if exact["present"] != true {
		t.Errorf("cache_stats exact present = %v, want true", exact["present"])
	}
	// Unwired layers report present=false, never zero-filled.
	semantic, _ := sc["semantic"].(map[string]any)
	if semantic["present"] != false {
		t.Errorf("cache_stats semantic present = %v, want false", semantic["present"])
	}
	persistent, _ := sc["persistent"].(map[string]any)
	if persistent["present"] != false {
		t.Errorf("cache_stats persistent present = %v, want false", persistent["present"])
	}
}

func TestOpsCacheFlushEvictsOnlyCallerTenant(t *testing.T) {
	s, src := newOpsTestServer(t)
	sc := callTool(t, s, "cache_flush", map[string]any{})
	if sc["flushed"] != float64(2) {
		t.Fatalf("cache_flush flushed = %v, want 2 (key-a's entries)", sc["flushed"])
	}
	if sc["tenant"] != "key-a" {
		t.Errorf("cache_flush tenant = %v, want key-a", sc["tenant"])
	}
	if sc["scope"] != "caller-only" {
		t.Errorf("cache_flush scope = %v, want caller-only", sc["scope"])
	}
	// The other tenant's entries survive, untouched.
	if keys, ok := src.cache["key-b"]; !ok || len(keys) != 1 || keys["k3"] != "other answer" {
		t.Errorf("key-b cache after flush = %v, want {k3: other answer}", src.cache["key-b"])
	}
	if _, ok := src.cache["key-a"]; ok {
		t.Errorf("key-a cache still present after flush: %v", src.cache["key-a"])
	}
	// Stats agree: one entry left, the other tenant's.
	stats := callTool(t, s, "cache_stats", map[string]any{})
	exact, _ := stats["exact"].(map[string]any)
	if exact["entries"] != float64(1) {
		t.Errorf("cache_stats after flush entries = %v, want 1", exact["entries"])
	}
}

func TestOpsCacheFlushDeniedWithoutDestructive(t *testing.T) {
	s, src := newOpsTestServer(t)
	narrow := serverFor(src, "key-a", []string{ScopeManageCache})
	if code := callToolErr(t, narrow, "cache_flush", map[string]any{}); code != float64(ScopeDeniedCode) {
		t.Errorf("cache_flush with only mcp:manage:cache: error code = %v, want %d", code, ScopeDeniedCode)
	}
	// Denial evicted nothing.
	if len(src.cache["key-a"]) != 2 {
		t.Errorf("denied flush changed key-a cache: %v", src.cache["key-a"])
	}
	_ = s
}

func TestOpsCacheFlushDeniedWithoutManageCache(t *testing.T) {
	_, src := newOpsTestServer(t)
	narrow := serverFor(src, "key-a", []string{ScopeDestructive})
	if code := callToolErr(t, narrow, "cache_flush", map[string]any{}); code != float64(ScopeDeniedCode) {
		t.Errorf("cache_flush with only mcp:destructive: error code = %v, want %d", code, ScopeDeniedCode)
	}
	if len(src.cache["key-a"]) != 2 {
		t.Errorf("denied flush changed key-a cache: %v", src.cache["key-a"])
	}
}

func TestOpsCacheStatsDeniedWithoutScope(t *testing.T) {
	_, src := newOpsTestServer(t)
	narrow := serverFor(src, "key-a", []string{ScopeManageMemory})
	if code := callToolErr(t, narrow, "cache_stats", map[string]any{}); code != float64(ScopeDeniedCode) {
		t.Errorf("cache_stats without mcp:manage:cache: error code = %v, want %d", code, ScopeDeniedCode)
	}
}

func TestOpsDBHealthReturnsLiveFixture(t *testing.T) {
	s, _ := newOpsTestServer(t)
	sc := callTool(t, s, "db_health", map[string]any{})
	if sc["healthy"] != true {
		t.Errorf("db_health healthy = %v, want true", sc["healthy"])
	}
	files, _ := sc["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("db_health files = %d, want 1", len(files))
	}
	f, _ := files[0].(map[string]any)
	if f["name"] != "ledger.db" || f["exists"] != true {
		t.Errorf("db_health file = %v, want ledger.db exists", f)
	}
	rows, _ := f["rows"].(map[string]any)
	if rows["usage_events"] != float64(7) {
		t.Errorf("db_health ledger usage_events = %v, want 7", rows["usage_events"])
	}
}

func TestOpsDBStatsReturnsSizes(t *testing.T) {
	s, _ := newOpsTestServer(t)
	sc := callTool(t, s, "db_stats", map[string]any{})
	files, _ := sc["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("db_stats files = %d, want 1", len(files))
	}
	f, _ := files[0].(map[string]any)
	if f["size_bytes"] != float64(1234) {
		t.Errorf("db_stats ledger size_bytes = %v, want 1234", f["size_bytes"])
	}
}

func TestOpsMemoryRoundTrip(t *testing.T) {
	s, _ := newOpsTestServer(t)
	added := callTool(t, s, "memory_add", map[string]any{"text": "prefers dark mode"})
	if added["id"] == nil || added["id"] == "" {
		t.Errorf("memory_add returned no id: %v", added)
	}
	callTool(t, s, "memory_add", map[string]any{"text": "writes Go every day"})
	// Case-insensitive substring search finds the Go note only.
	found := callTool(t, s, "memory_search", map[string]any{"query": "GO"})
	if found["count"] != float64(1) {
		t.Fatalf("memory_search GO count = %v, want 1", found["count"])
	}
	entries, _ := found["entries"].([]any)
	e, _ := entries[0].(map[string]any)
	if e["text"] != "writes Go every day" {
		t.Errorf("memory_search entry = %v, want the Go note", e["text"])
	}
	if e["matches"] != float64(1) {
		t.Errorf("memory_search matches = %v, want 1 (one occurrence of GO)", e["matches"])
	}
	// Clear removes both of the caller's entries; a search after
	// finds nothing.
	cleared := callTool(t, s, "memory_clear", map[string]any{})
	if cleared["cleared"] != float64(2) {
		t.Errorf("memory_clear cleared = %v, want 2", cleared["cleared"])
	}
	after := callTool(t, s, "memory_search", map[string]any{"query": "Go"})
	if after["count"] != float64(0) {
		t.Errorf("memory_search after clear count = %v, want 0", after["count"])
	}
}

func TestOpsMemorySearchRankingIsOccurrenceCount(t *testing.T) {
	s, _ := newOpsTestServer(t)
	callTool(t, s, "memory_add", map[string]any{"text": "go once"})
	callTool(t, s, "memory_add", map[string]any{"text": "go go go everywhere"})
	found := callTool(t, s, "memory_search", map[string]any{"query": "go"})
	if found["count"] != float64(2) {
		t.Fatalf("memory_search count = %v, want 2", found["count"])
	}
	entries, _ := found["entries"].([]any)
	first, _ := entries[0].(map[string]any)
	second, _ := entries[1].(map[string]any)
	// Ranking is the raw occurrence count, most first — no fake
	// relevance score: the 3-occurrence entry outranks the 1.
	if first["matches"] != float64(3) || first["text"] != "go go go everywhere" {
		t.Errorf("first search result = %v, want the 3-occurrence entry", first)
	}
	if second["matches"] != float64(1) {
		t.Errorf("second search result matches = %v, want 1", second["matches"])
	}
}

func TestOpsMemoryIsolatedPerCaller(t *testing.T) {
	s, src := newOpsTestServer(t)
	callTool(t, s, "memory_add", map[string]any{"text": "key-a secret note"})
	other := serverFor(src, "key-b", []string{ScopeManageMemory, ScopeDestructive})
	// key-b cannot see key-a's memory.
	found := callTool(t, other, "memory_search", map[string]any{"query": "secret"})
	if found["count"] != float64(0) {
		t.Errorf("key-b memory_search found key-a's entry: %v", found)
	}
	// key-b's clear deletes nothing and leaves key-a's entry.
	cleared := callTool(t, other, "memory_clear", map[string]any{})
	if cleared["cleared"] != float64(0) {
		t.Errorf("key-b memory_clear cleared = %v, want 0", cleared["cleared"])
	}
	mine := callTool(t, s, "memory_search", map[string]any{"query": "secret"})
	if mine["count"] != float64(1) {
		t.Errorf("key-a memory_search after key-b clear count = %v, want 1", mine["count"])
	}
}

func TestOpsMemoryClearDeniedWithoutDestructive(t *testing.T) {
	s, src := newOpsTestServer(t)
	callTool(t, s, "memory_add", map[string]any{"text": "keep me"})
	narrow := serverFor(src, "key-a", []string{ScopeManageMemory})
	if code := callToolErr(t, narrow, "memory_clear", map[string]any{}); code != float64(ScopeDeniedCode) {
		t.Errorf("memory_clear with only mcp:manage:memory: error code = %v, want %d", code, ScopeDeniedCode)
	}
	after := callTool(t, s, "memory_search", map[string]any{"query": "keep"})
	if after["count"] != float64(1) {
		t.Errorf("denied memory_clear deleted the entry: %v", after)
	}
}

func TestOpsMemoryClearDeniedWithoutManageMemory(t *testing.T) {
	s, src := newOpsTestServer(t)
	callTool(t, s, "memory_add", map[string]any{"text": "keep me"})
	narrow := serverFor(src, "key-a", []string{ScopeDestructive})
	if code := callToolErr(t, narrow, "memory_clear", map[string]any{}); code != float64(ScopeDeniedCode) {
		t.Errorf("memory_clear with only mcp:destructive: error code = %v, want %d", code, ScopeDeniedCode)
	}
	after := callTool(t, s, "memory_search", map[string]any{"query": "keep"})
	if after["count"] != float64(1) {
		t.Errorf("denied memory_clear deleted the entry: %v", after)
	}
}

func TestOpsMemoryAddRequiresText(t *testing.T) {
	s, _ := newOpsTestServer(t)
	if code := callToolErr(t, s, "memory_add", map[string]any{"text": "   "}); code != float64(-32602) {
		t.Errorf("memory_add with blank text: error code = %v, want -32602", code)
	}
	if code := callToolErr(t, s, "memory_add", map[string]any{}); code != float64(-32602) {
		t.Errorf("memory_add with no text: error code = %v, want -32602", code)
	}
	if code := callToolErr(t, s, "memory_add", map[string]any{"text": strings.Repeat("x", MaxMemoryTextLen+1)}); code != float64(-32602) {
		t.Errorf("memory_add over the length limit: error code = %v, want -32602", code)
	}
}

func TestOpsMemoryAddLimitCountsRunesNotBytes(t *testing.T) {
	s, _ := newOpsTestServer(t)
	// 10,000 CJK runes is 30,000 bytes: exactly at the rune
	// limit, it must be ACCEPTED. A byte count would reject it
	// as 3x over the limit — that is the bug this test pins.
	if code := callToolErr(t, s, "memory_add", map[string]any{"text": strings.Repeat("日", MaxMemoryTextLen)}); code != 0 {
		t.Errorf("memory_add with %d CJK runes (30,000 bytes): error code = %v, want 0 (accepted)", MaxMemoryTextLen, code)
	}
	// One rune over the limit must still be rejected, even
	// though it is multibyte: the boundary is runes.
	if code := callToolErr(t, s, "memory_add", map[string]any{"text": strings.Repeat("😀", MaxMemoryTextLen+1)}); code != float64(-32602) {
		t.Errorf("memory_add with %d emoji runes: error code = %v, want -32602", MaxMemoryTextLen+1, code)
	}
	// And the accepted CJK entry must round-trip through
	// search: its JSON line is ~30 KB, past the old scanner
	// buffer, so this also pins the read-back path.
	found := callTool(t, s, "memory_search", map[string]any{"query": "日"})
	if found["count"] != float64(1) {
		t.Errorf("memory_search for the CJK entry: count = %v, want 1", found["count"])
	}
}

func TestOpsMemorySearchRequiresQuery(t *testing.T) {
	s, _ := newOpsTestServer(t)
	if code := callToolErr(t, s, "memory_search", map[string]any{"query": ""}); code != float64(-32602) {
		t.Errorf("memory_search with empty query: error code = %v, want -32602", code)
	}
}

// H4 — path leaks: a store/DB error must never reach a remote
// caller verbatim, because os errors embed the full local path.
// pathLeakSource is a Source whose ops fail with realistic
// path-bearing errors (an os.Stat *PathError, an open %q error);
// the tools/call reply must carry only the fixed path-free
// message, never the directory.
type pathLeakSource struct {
	*fakeSource
	cacheFlushErr error
	memoryErr     error
}

func (s *pathLeakSource) CacheFlush(tenant string) (int, error) {
	return 0, s.cacheFlushErr
}

func (s *pathLeakSource) MemoryAdd(caller, text string) (MemoryEntry, error) {
	return MemoryEntry{}, s.memoryErr
}

func (s *pathLeakSource) MemorySearch(caller, query string) ([]MemoryEntry, error) {
	return nil, s.memoryErr
}

func (s *pathLeakSource) MemoryClear(caller string) (int, error) {
	return 0, s.memoryErr
}

func leakServer(t *testing.T, src *pathLeakSource, scopes []string) *Server {
	t.Helper()
	return New(src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-a", scopes))
}

func TestOpsMemoryAddStoreErrorIsPathFree(t *testing.T) {
	_, src := newOpsTestServer(t)
	leakPath := filepath.Join(t.TempDir(), "mcp", "memory.jsonl")
	leak := &pathLeakSource{fakeSource: src, memoryErr: fmt.Errorf("mcp: memory store open %q: permission denied", leakPath)}
	s := leakServer(t, leak, []string{ScopeManageMemory})
	code, msg := callToolErrMsg(t, s, "memory_add", map[string]any{"text": "hello"})
	if code != float64(-32602) {
		t.Fatalf("memory_add store error: code = %v, want -32602", code)
	}
	if msg != "unavailable: memory store unavailable" {
		t.Errorf("memory_add store error message = %q, want %q", msg, "unavailable: memory store unavailable")
	}
	if strings.Contains(msg, leakPath) || strings.Contains(msg, t.TempDir()) {
		t.Errorf("memory_add store error leaks the path: %q", msg)
	}
}

func TestOpsMemorySearchStoreErrorIsPathFree(t *testing.T) {
	_, src := newOpsTestServer(t)
	leakPath := filepath.Join(t.TempDir(), "mcp", "memory.jsonl")
	leak := &pathLeakSource{fakeSource: src, memoryErr: fmt.Errorf("mcp: memory store read %q: permission denied", leakPath)}
	s := leakServer(t, leak, []string{ScopeManageMemory})
	code, msg := callToolErrMsg(t, s, "memory_search", map[string]any{"query": "hello"})
	if code != float64(-32602) {
		t.Fatalf("memory_search store error: code = %v, want -32602", code)
	}
	if msg != "unavailable: memory store unavailable" {
		t.Errorf("memory_search store error message = %q, want %q", msg, "unavailable: memory store unavailable")
	}
	if strings.Contains(msg, leakPath) {
		t.Errorf("memory_search store error leaks the path: %q", msg)
	}
}

func TestOpsMemoryClearStoreErrorIsPathFree(t *testing.T) {
	_, src := newOpsTestServer(t)
	leakPath := filepath.Join(t.TempDir(), "mcp", "memory.jsonl")
	leak := &pathLeakSource{fakeSource: src, memoryErr: fmt.Errorf("mcp: memory_clear replace: rename %q: permission denied", leakPath)}
	s := leakServer(t, leak, []string{ScopeManageMemory, ScopeDestructive})
	code, msg := callToolErrMsg(t, s, "memory_clear", map[string]any{})
	if code != float64(-32602) {
		t.Fatalf("memory_clear store error: code = %v, want -32602", code)
	}
	if msg != "unavailable: memory store unavailable" {
		t.Errorf("memory_clear store error message = %q, want %q", msg, "unavailable: memory store unavailable")
	}
	if strings.Contains(msg, leakPath) {
		t.Errorf("memory_clear store error leaks the path: %q", msg)
	}
}

func TestOpsCacheFlushStoreErrorIsPathFree(t *testing.T) {
	_, src := newOpsTestServer(t)
	leakPath := filepath.Join(t.TempDir(), "cache.db")
	leak := &pathLeakSource{fakeSource: src, cacheFlushErr: fmt.Errorf("stat %s: no such file or directory", leakPath)}
	s := leakServer(t, leak, []string{ScopeManageCache, ScopeDestructive})
	code, msg := callToolErrMsg(t, s, "cache_flush", map[string]any{})
	if code != float64(-32602) {
		t.Fatalf("cache_flush store error: code = %v, want -32602", code)
	}
	if msg != "unavailable: persistent cache unavailable" {
		t.Errorf("cache_flush store error message = %q, want %q", msg, "unavailable: persistent cache unavailable")
	}
	if strings.Contains(msg, leakPath) || strings.Contains(msg, t.TempDir()) {
		t.Errorf("cache_flush store error leaks the path: %q", msg)
	}
}

// TestOpsMemoryAddUnusableStoreIsPathFree is the proven H4
// repro end to end: a memory_add while the store is unusable
// (its directory is a regular file, so every filesystem call
// fails with a *PathError carrying the full data-dir path)
// must answer -32602 with the fixed path-free message — the
// full path reaches only the server log, never the caller.
func TestOpsMemoryAddUnusableStoreIsPathFree(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	src := &fakeSource{
		providers: []ProviderInfo{{ID: "p1", Name: "Coder", WireFormat: "openai", Enabled: true, Models: []string{"coder-model"}}},
		models:    []ModelInfo{{ID: "coder-model", ProviderID: "p1", ProviderName: "Coder"}},
		status:    StatusInfo{Version: "0.1.0"},
		cache:     map[string]map[string]string{},
		mem:       NewMemoryStore(filepath.Join(blocker, "memory.jsonl")),
	}
	s := New(src, "0.1.0").WithClientIdentity(GatewayKeyClient("key-a", []string{ScopeManageMemory}))
	code, msg := callToolErrMsg(t, s, "memory_add", map[string]any{"text": "hello"})
	if code != float64(-32602) {
		t.Fatalf("memory_add on unusable store: code = %v, want -32602", code)
	}
	if msg != "unavailable: memory store unavailable" {
		t.Errorf("memory_add on unusable store: message = %q, want %q", msg, "unavailable: memory store unavailable")
	}
	if strings.Contains(msg, dir) {
		t.Errorf("memory_add on unusable store leaks the path: %q", msg)
	}
}

// The over-limit rejection is an invalid input, not a store
// failure: it must still explain the limit to the caller
// instead of collapsing into the generic unavailable message.
func TestOpsMemoryOverLimitErrorStillExplainsLimit(t *testing.T) {
	s, _ := newOpsTestServer(t)
	code, msg := callToolErrMsg(t, s, "memory_add", map[string]any{"text": strings.Repeat("x", MaxMemoryTextLen+1)})
	if code != float64(-32602) {
		t.Fatalf("memory_add over the limit: code = %v, want -32602", code)
	}
	if !strings.Contains(msg, "over the 10000 limit") {
		t.Errorf("memory_add over the limit: message = %q, want it to name the 10000 limit", msg)
	}
	if msg == "unavailable: memory store unavailable" {
		t.Errorf("memory_add over the limit collapsed into the generic unavailable message: %q", msg)
	}
}
