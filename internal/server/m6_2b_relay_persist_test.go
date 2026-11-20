// M6.2b — relay write-through: server.WithCaches accepts the
// persistent wrappers, so the relay's independent store path
// (storeMessagesCache) writes through to disk synchronously,
// exactly like the engine path.
//
// Toggle-verification: these tests FAIL against the pre-M6.2b
// code (restored from backup) — WithCaches takes concrete
// *cache.ExactCache / *cache.LocalSemanticCache there, so
// passing a *cache.PersistentCache (or psc.AsStore()) does not
// compile.
package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/cache"
)

// TestM62bWithCachesTypedNilIsAbsent is the regression test for
// the interface-typed-nil hazard on the relay side: typed nil
// caches passed to WithCaches must behave like absent layers
// (lookup misses, no panic), not like present caches with a nil
// receiver.
func TestM62bWithCachesTypedNilIsAbsent(t *testing.T) {
	var nilExact *cache.ExactCache
	var nilSemantic *cache.LocalSemanticCache
	srv := newTestServer(t, &fakeEngine{}, WithCaches(nilExact, nilSemantic))
	if _, _, found := srv.lookupMessagesCache(context.Background(), m64RelayReq(nil)); found {
		t.Fatal("relay lookup hit with typed-nil caches; want miss")
	}
}

// TestM62bRelayStoreWritesThrough proves the /v1/messages relay
// stores verbatim responses through to disk: store once via the
// relay helper, close both files with no Sync, reopen, and the
// entries are there on both layers.
func TestM62bRelayStoreWritesThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	psc, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("OpenPersistentSemanticCache: %v", err)
	}
	s := &Server{exactCache: pc, semanticCache: psc.AsStore()}
	body := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"4"}],"model":"test-model","stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`
	s.storeMessagesCache(ctx, m64RelayReq(nil), []byte(body))
	if hit, layer, found := s.lookupMessagesCache(ctx, m64RelayReq(nil)); !found || layer != "exact" || hit.Content != "4" {
		t.Fatalf("live relay lookup = %+v,%q,%v, want 4,exact,true", hit, layer, found)
	}
	// No Sync anywhere: close both files and reopen.
	if err := pc.Close(); err != nil {
		t.Fatalf("pc Close: %v", err)
	}
	if err := psc.Close(); err != nil {
		t.Fatalf("psc Close: %v", err)
	}
	pc2, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("reopen exact: %v", err)
	}
	defer pc2.Close()
	psc2, err := cache.OpenPersistentSemanticCache(filepath.Join(dir, "sem.db"), cache.LocalSemanticCacheConfig{})
	if err != nil {
		t.Fatalf("reopen semantic: %v", err)
	}
	defer psc2.Close()
	s2 := &Server{exactCache: pc2, semanticCache: psc2.AsStore()}
	if hit, layer, found := s2.lookupMessagesCache(ctx, m64RelayReq(nil)); !found || layer != "exact" || hit.Content != "4" {
		t.Fatalf("reopened relay lookup = %+v,%q,%v, want 4,exact,true (relay store did not write through)", hit, layer, found)
	}
	if got, ok := psc2.GetTextForModel(ctx, "test-model", "What is 2+2?"); !ok || got.Response.Content != "4" {
		t.Fatalf("reopened semantic GetTextForModel = %+v, %v; want hit with content %q", got, ok, "4")
	}
}

// TestM62bRelayLookupReadsPersistent proves the relay lookup
// reads entries the persistent exact cache already holds (e.g.
// stored by the engine path): the two dialects share one store.
func TestM62bRelayLookupReadsPersistent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pc, err := cache.OpenPersistentCache(filepath.Join(dir, "exact.db"), 500)
	if err != nil {
		t.Fatalf("OpenPersistentCache: %v", err)
	}
	defer pc.Close()
	s := &Server{exactCache: pc}
	req := m64RelayReq(nil)
	key, err := scopedMessagesKey(req)
	if err != nil {
		t.Fatalf("scopedMessagesKey: %v", err)
	}
	pc.Set(ctx, key, cache.CachedResponse{Content: "4", Model: "test-model"}, time.Hour)
	if hit, layer, found := s.lookupMessagesCache(ctx, req); !found || layer != "exact" || hit.Content != "4" {
		t.Fatalf("relay lookup = %+v,%q,%v, want 4,exact,true", hit, layer, found)
	}
}
