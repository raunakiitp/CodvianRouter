// M8.14 race soak: concurrent combo mutations against every
// live reader of cfg.Routing.Groups / cfg.Routing.ChainStrategies.
//
// The merge reviewer proved with -race that concurrent combo
// mutation + group traffic raced: mirrorGroup's map write
// (combos_api.go) against groupChainFor's map read (main.go).
// The fix is one shared sync.RWMutex on *config.Config
// (config.routingMu), taken by every accessor. This test pins
// it: four writer goroutines drive the live combos HTTP handler
// (POST/PATCH/DELETE, i.e. mirrorGroup / mirrorStrategy /
// unmirror) while reader goroutines hammer the same maps through
// the fixed read paths — GroupMembers / GroupsSnapshot (the
// groupChainFor and GET /dashboard/groups reads), Clone (the
// keystatus/danger live-request path), ChainStrategy /
// ChainStrategiesSnapshot (the engine ChainStrategy closure
// and strategyForModel reads), and groupsFeed itself.
//
// It must FAIL under -race when the mutex is neutered (toggle:
// the four lock methods stubbed to no-ops, so every access is
// unlocked exactly as in the pre-fix code) and PASS with the
// real mutex. It asserts nothing about the final combo state
// (writers end mid-cycle by design) — only that the run is
// race-free and the store still answers afterwards.
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCombosRaceSoak(t *testing.T) {
	srv, _, _, _ := groupStack(t)
	cfg := srv.cfg
	store := NewComboStore(cfg, nil)
	h := store.Handler()

	// One writer per combo name, so writers never fight each
	// other over a record — every conflict in this test is
	// writer-vs-reader on the shared routing maps.
	const writers = 4
	deadline := time.Now().Add(3 * time.Second)
	start := make(chan struct{})
	var wg sync.WaitGroup

	do := func(method, path, body string) int {
		var rd strings.Reader
		if body != "" {
			rd = *strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, &rd)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("race-%d", w)
			create := fmt.Sprintf(`{"name":%q,"chain":[{"provider":"a"},{"provider":"b"}],"strategy":"cheapest"}`, name)
			for time.Now().Before(deadline) {
				// Create (201, or 409 when the previous
				// cycle's delete has not landed — both
				// are the live mirror path).
				if code := do(http.MethodPost, "/v1/combos", create); code != http.StatusCreated && code != http.StatusConflict {
					t.Errorf("writer %d: create status %d", w, code)
					return
				}
				// Patch to a record-only execution mode
				// (drops the strategy mirror) and back
				// (restores it): both mirror branches.
				if code := do(http.MethodPatch, "/v1/combos/"+name, `{"strategy":"fusion"}`); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: patch fusion status %d", w, code)
					return
				}
				if code := do(http.MethodPatch, "/v1/combos/"+name, `{"strategy":"priority"}`); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: patch priority status %d", w, code)
					return
				}
				if code := do(http.MethodGet, "/v1/combos/"+name, ""); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: get status %d", w, code)
					return
				}
				if code := do(http.MethodDelete, "/v1/combos/"+name, ""); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: delete status %d", w, code)
					return
				}
			}
		}(w)
	}

	// Readers: the groupChainFor-style lookup + the groups-feed
	// snapshot, on the same maps the writers mirror into.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			for i := 0; i < writers; i++ {
				name := fmt.Sprintf("race-%d", i)
				if members, ok := cfg.GroupMembers(name); ok && len(members) != 2 {
					t.Errorf("GroupMembers(%q) = %v, want 2 members", name, members)
					return
				}
			}
			_ = cfg.GroupsSnapshot()
		}
	}()

	// Readers: Clone, the live-request path (keystatus/danger).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			clone := cfg.Clone()
			if clone == nil || clone.Routing == nil {
				t.Errorf("Clone lost the routing block")
				return
			}
		}
	}()

	// Readers: the chain-strategies map (engine closure +
	// strategyForModel).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			for i := 0; i < writers; i++ {
				name := fmt.Sprintf("race-%d", i)
				_, _ = cfg.ChainStrategy(name)
				_, _, _ = srv.strategyForModel(name)
			}
			_ = cfg.ChainStrategiesSnapshot()
		}
	}()

	// Reader: the groups feed itself, against the same cfg the
	// store mutates. One goroutine only: the point is the feed
	// racing the writers, not the feed racing itself.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			_ = srv.groupsFeed()
		}
	}()

	close(start)
	wg.Wait()

	// The store still answers after the soak.
	if code := do(http.MethodGet, "/v1/combos", ""); code != http.StatusOK {
		t.Fatalf("list after soak: status %d, want 200", code)
	}
	// Every surviving group in the snapshot belongs to a combo
	// that still exists, or to the pre-existing "fast" group
	// groupStack seeds.
	combos := map[string]bool{}
	for _, c := range store.records {
		combos[c.name] = true
	}
	for name := range cfg.GroupsSnapshot() {
		if name != "fast" && !combos[name] {
			t.Fatalf("snapshot group %q has no combo record", name)
		}
	}
}
