// Per-request identity threading (M9.2 coordinator follow-up).
//
// Tool handlers read the caller from s.identity (see callerID in
// tools_ops.go: the tenant for cache_flush, the namespace for the
// memory tools). handleWithIdentity used to dispatch on the shared
// server, so a remote transport's per-request GatewayKeyClient
// identity never reached the handlers: every remote key's
// cache_flush flushed the "anonymous" tenant and its memories
// landed in the wrong namespace. The fix dispatches through a
// shallow server copy carrying the per-request identity (see
// withIdentity in mcp.go). These tests prove the copy reaches the
// handlers — sequentially, concurrently, and for the server
// default — using the tools whose outputs name the caller.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// callWithIdentity is call() for a per-request identity: it runs
// one tools/call through HandleWithIdentity and returns the
// parsed result object (failing the test on any JSON-RPC error).
func callWithIdentity(t *testing.T, s *Server, id ClientIdentity, tool string, args string) map[string]any {
	t.Helper()
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool, args)
	raw := s.HandleWithIdentity(context.Background(), []byte(msg), id)
	if raw == nil {
		t.Fatalf("HandleWithIdentity(%s) returned nil, want a response", tool)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	if errObj, ok := out["error"].(map[string]any); ok {
		t.Fatalf("tools/call %s returned JSON-RPC error: %v", tool, errObj)
	}
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s result missing: %v", tool, out)
	}
	return result
}

// structuredContent pulls the toolResult payload out of a
// tools/call result: the same payload the content[0].text
// carries, as a map.
func structuredContent(t *testing.T, result map[string]any) map[string]any {
	t.Helper()
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent missing: %v", result)
	}
	return sc
}

func cacheFlushIdentity(keyID string) ClientIdentity {
	return GatewayKeyClient(keyID, []string{ScopeManageCache, ScopeDestructive})
}

func memoryIdentity(keyID string) ClientIdentity {
	return GatewayKeyClient(keyID, []string{ScopeManageMemory, ScopeDestructive})
}

// TestHandleWithIdentityReachesHandlersCacheFlush proves the
// per-request identity reaches the cache_flush handler: each key
// flushes exactly its own tenant, and the result names that
// tenant. Before the withIdentity fix this failed: every call
// flushed tenant "anonymous" (0 entries) and left both tenants
// untouched.
func TestHandleWithIdentityReachesHandlersCacheFlush(t *testing.T) {
	src := &fakeSource{cache: map[string]map[string]string{
		"key-A": {"k1": "v1", "k2": "v2"},
		"key-B": {"k1": "v1"},
	}}
	srv := New(src, "test")

	res := callWithIdentity(t, srv, cacheFlushIdentity("key-A"), "cache_flush", `{}`)
	sc := structuredContent(t, res)
	if sc["tenant"] != "key-A" {
		t.Errorf("cache_flush tenant = %v, want key-A (per-request identity did not reach the handler)", sc["tenant"])
	}
	if sc["flushed"] != float64(2) {
		t.Errorf("cache_flush flushed = %v, want 2 (key-A's entries)", sc["flushed"])
	}

	res = callWithIdentity(t, srv, cacheFlushIdentity("key-B"), "cache_flush", `{}`)
	sc = structuredContent(t, res)
	if sc["tenant"] != "key-B" {
		t.Errorf("cache_flush tenant = %v, want key-B", sc["tenant"])
	}
	if sc["flushed"] != float64(1) {
		t.Errorf("cache_flush flushed = %v, want 1 (key-B's entries)", sc["flushed"])
	}

	// Both tenants are now empty: a repeat flush finds nothing,
	// and the other tenant's entries were never touched by the
	// first key's flush (key-B still flushed 1 above).
	res = callWithIdentity(t, srv, cacheFlushIdentity("key-A"), "cache_flush", `{}`)
	if sc := structuredContent(t, res); sc["flushed"] != float64(0) {
		t.Errorf("second cache_flush for key-A flushed = %v, want 0", sc["flushed"])
	}
}

// TestHandleWithIdentityReachesHandlersMemoryIsolation proves
// the per-request identity namespaces the memory tools: a memory
// stored by one key is invisible to another key's search, and a
// clear by one key deletes only its own entries.
func TestHandleWithIdentityReachesHandlersMemoryIsolation(t *testing.T) {
	src := &fakeSource{mem: NewMemoryStore(filepath.Join(t.TempDir(), "memory.jsonl"))}
	srv := New(src, "test")

	res := callWithIdentity(t, srv, memoryIdentity("key-A"), "memory_add", `{"text":"hello world"}`)
	if sc := structuredContent(t, res); sc["text"] != "hello world" {
		t.Errorf("memory_add text = %v, want hello world", sc["text"])
	}

	// Key B must not see key A's memory.
	res = callWithIdentity(t, srv, memoryIdentity("key-B"), "memory_search", `{"query":"hello"}`)
	if sc := structuredContent(t, res); sc["count"] != float64(0) {
		t.Errorf("key-B memory_search count = %v, want 0 (key-A's memory leaked across callers)", sc["count"])
	}

	// Key A sees its own memory.
	res = callWithIdentity(t, srv, memoryIdentity("key-A"), "memory_search", `{"query":"hello"}`)
	if sc := structuredContent(t, res); sc["count"] != float64(1) {
		t.Errorf("key-A memory_search count = %v, want 1", sc["count"])
	}

	// Key B's clear deletes nothing of A's.
	res = callWithIdentity(t, srv, memoryIdentity("key-B"), "memory_clear", `{}`)
	if sc := structuredContent(t, res); sc["cleared"] != float64(0) {
		t.Errorf("key-B memory_clear cleared = %v, want 0", sc["cleared"])
	}
	res = callWithIdentity(t, srv, memoryIdentity("key-A"), "memory_search", `{"query":"hello"}`)
	if sc := structuredContent(t, res); sc["count"] != float64(1) {
		t.Errorf("key-A memory_search after key-B clear count = %v, want 1", sc["count"])
	}

	// Key A's own clear removes it.
	res = callWithIdentity(t, srv, memoryIdentity("key-A"), "memory_clear", `{}`)
	if sc := structuredContent(t, res); sc["cleared"] != float64(1) {
		t.Errorf("key-A memory_clear cleared = %v, want 1", sc["cleared"])
	}
}

// lockingFakeSource serialises CacheFlush (the embedded fake's
// tenant map is not safe for concurrent use) and records every
// tenant it was asked to flush, so the concurrent test can prove
// each request flushed exactly its own tenant.
type lockingFakeSource struct {
	*fakeSource
	mu           sync.Mutex
	flushTenants []string
}

func (f *lockingFakeSource) CacheFlush(tenant string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushTenants = append(f.flushTenants, tenant)
	return f.fakeSource.CacheFlush(tenant)
}

// TestHandleWithIdentityConcurrentNoCrossTalk proves identities
// never cross between concurrent requests on one shared server:
// 16 goroutines, each with its own key, each flush exactly its
// own tenant — and the shared server's configured identity is
// still Anonymous afterwards (the fix never mutates it).
func TestHandleWithIdentityConcurrentNoCrossTalk(t *testing.T) {
	const n = 16
	cache := make(map[string]map[string]string, n)
	for i := 0; i < n; i++ {
		cache[fmt.Sprintf("key-%02d", i)] = map[string]string{"k": "v"}
	}
	src := &lockingFakeSource{fakeSource: &fakeSource{cache: cache}}
	srv := New(src, "test")

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("key-%02d", i)
			msg := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`
			raw := srv.HandleWithIdentity(context.Background(), []byte(msg), cacheFlushIdentity(id))
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				errs[i] = fmt.Errorf("key %s: response is not JSON: %v", id, err)
				return
			}
			if errObj, ok := out["error"].(map[string]any); ok {
				errs[i] = fmt.Errorf("key %s: JSON-RPC error: %v", id, errObj)
				return
			}
			result, ok := out["result"].(map[string]any)
			if !ok {
				errs[i] = fmt.Errorf("key %s: result missing", id)
				return
			}
			sc, ok := result["structuredContent"].(map[string]any)
			if !ok {
				errs[i] = fmt.Errorf("key %s: structuredContent missing", id)
				return
			}
			if sc["tenant"] != id {
				errs[i] = fmt.Errorf("key %s: handler saw tenant %v (identity crossed requests)", id, sc["tenant"])
				return
			}
			if sc["flushed"] != float64(1) {
				errs[i] = fmt.Errorf("key %s: flushed %v, want 1", id, sc["flushed"])
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Error(err)
		}
		_ = i
	}

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.flushTenants) != n {
		t.Fatalf("CacheFlush called %d times, want %d", len(src.flushTenants), n)
	}
	seen := make(map[string]int, n)
	for _, tenant := range src.flushTenants {
		seen[tenant]++
	}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("key-%02d", i)
		if seen[id] != 1 {
			t.Errorf("tenant %s flushed %d times, want 1 (another request flushed it instead)", id, seen[id])
		}
	}

	// The shared server was never mutated: its configured
	// identity is still the Anonymous default.
	if srv.identity.Kind != KindAnonymous || srv.identity.ID != "anonymous" {
		t.Errorf("shared server identity mutated to %+v, want Anonymous", srv.identity)
	}
}

// TestHandleUsesConfiguredIdentity proves the copy path preserves
// the server-default behaviour for Handle (stdio): a server that
// opted into LocalOperator still dispatches handlers as the local
// operator, and a bare New() server still denies (Anonymous has
// no scopes, so the scope check fires before dispatch).
func TestHandleUsesConfiguredIdentity(t *testing.T) {
	src := &fakeSource{cache: map[string]map[string]string{
		"local-operator": {"k": "v"},
	}}
	srv := New(src, "test").WithClientIdentity(LocalOperator())
	res := call(t, srv, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`)
	result, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call result missing: %v", res)
	}
	sc := structuredContent(t, result)
	if sc["tenant"] != "local-operator" {
		t.Errorf("stdio cache_flush tenant = %v, want local-operator", sc["tenant"])
	}
	if sc["flushed"] != float64(1) {
		t.Errorf("stdio cache_flush flushed = %v, want 1", sc["flushed"])
	}

	// Bare New() (Anonymous): the scope check denies before any
	// handler runs, so no tenant is flushed.
	anon := New(&fakeSource{cache: map[string]map[string]string{
		"anonymous": {"k": "v"},
	}}, "test")
	raw := anon.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"cache_flush","arguments":{}}}`))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("anonymous tools/call succeeded, want -32002 denial: %v", out)
	}
	if errObj["code"] != float64(ScopeDeniedCode) {
		t.Errorf("anonymous tools/call code = %v, want %d", errObj["code"], ScopeDeniedCode)
	}
}
