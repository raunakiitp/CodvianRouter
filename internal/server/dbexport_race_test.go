// M8.14 race fix, round 2: dbexport + pointer-swap soak.
//
// The first round fixed the combo-mirror writes against the
// locked map readers, but POST /admin/db/export still encoded
// the LIVE cfg.Routing block (dbexport.go: the encoder ranged
// Groups / ChainStrategies with no lock), and the strategies /
// cooldown feeds and strategyForModel still derefed cfg.Routing
// directly, racing routingPersist's pointer swap. The
// re-reviewer proved both with -race: 31 DATA RACE reports
// and a panic (`reflect: call of reflect.Value.Set on zero
// Value` in encoding/json/v2) while encoding a map being
// mutated. The fix is RoutingSnapshot — one deep copy of the
// whole routing block under the routing read lock — used by
// the export encoder, the feeds and strategyForModel.
//
// This test pins it: four writer goroutines drive the live
// combos HTTP handler (the mirror writers) while a fifth
// goroutine runs the routingPersist-style swap loop (copy +
// pointer swap under the write lock, with a rollback every
// eighth swap, mimicking a failed Save), and reader
// goroutines hammer the export encoder, both feeds,
// strategyForModel, Clone and the group snapshots on the
// same cfg.
//
// It must FAIL under -race with the fix reverted (toggle: the
// export encoding cfg.Routing directly, the feeds and
// strategyForModel derefing it directly) and PASS with
// RoutingSnapshot. It asserts nothing about the final combo
// state (writers end mid-cycle by design) — only that the
// run is race-free, every export answers 200, and the store
// still answers afterwards.
package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

func TestDBExportRaceSoak(t *testing.T) {
	srv, _, _, raw := groupStack(t)
	cfg := srv.cfg
	// The export is a Tier 2 route: it needs an admin-scoped
	// key plus the typed EXPORT phrase (groupStack's key is
	// chat-only). Appended single-threaded, before any
	// goroutine starts.
	adminKey, adminRaw, err := config.IssueKey("test-admin", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GatewayKeys = append(cfg.GatewayKeys, adminKey)
	// Seed every map the export encoder ranges, so the soak
	// exercises all of them (groupStack seeds Groups only).
	cfg.Routing.GlobalStrategy = "balanced"
	cfg.Routing.ChainStrategies = map[string]string{"m": "cheapest"}
	cfg.Routing.ChainCooldownModes = map[string]string{"m": "hard"}
	cfg.Routing.ChainWeights = map[string]map[string]float64{"m": {"a/m": 2}}
	cfg.Routing.RefusalChains = map[string][]string{"m": {"a", "b"}}
	cfg.Routing.ModelEquivalents = map[string][]string{"m": {"m2"}}
	cfg.Routing.ContextFallbackModels = []string{"m"}

	store := NewComboStore(cfg, nil)
	h := store.Handler()

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
				if code := do(http.MethodPost, "/v1/combos", create); code != http.StatusCreated && code != http.StatusConflict {
					t.Errorf("writer %d: create status %d", w, code)
					return
				}
				if code := do(http.MethodPatch, "/v1/combos/"+name, `{"strategy":"fusion"}`); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: patch fusion status %d", w, code)
					return
				}
				if code := do(http.MethodPatch, "/v1/combos/"+name, `{"strategy":"priority"}`); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: patch priority status %d", w, code)
					return
				}
				if code := do(http.MethodDelete, "/v1/combos/"+name, ""); code != http.StatusOK && code != http.StatusNotFound {
					t.Errorf("writer %d: delete status %d", w, code)
					return
				}
			}
		}(w)
	}

	// routingPersist-style swap loop (main.go): copy the live
	// block under the write lock — deep-copying exactly the
	// maps routingPersist copies — flip the global strategy,
	// swap the pointer, release before any I/O; every eighth
	// swap rolls the pointer back, mimicking a failed Save.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		flip := false
		swaps := 0
		for time.Now().Before(deadline) {
			cfg.LockRouting()
			rc := &config.RoutingConfig{}
			if cfg.Routing != nil {
				*rc = *cfg.Routing
				if cfg.Routing.ChainStrategies != nil {
					rc.ChainStrategies = make(map[string]string, len(cfg.Routing.ChainStrategies))
					for k, v := range cfg.Routing.ChainStrategies {
						rc.ChainStrategies[k] = v
					}
				}
				if cfg.Routing.WeightedWeights != nil {
					w := *cfg.Routing.WeightedWeights
					rc.WeightedWeights = &w
				}
				if cfg.Routing.ChainCooldownModes != nil {
					rc.ChainCooldownModes = make(map[string]string, len(cfg.Routing.ChainCooldownModes))
					for k, v := range cfg.Routing.ChainCooldownModes {
						rc.ChainCooldownModes[k] = v
					}
				}
			}
			flip = !flip
			if flip {
				rc.GlobalStrategy = "cheapest"
			} else {
				rc.GlobalStrategy = "balanced"
			}
			old := cfg.Routing
			cfg.Routing = rc
			cfg.UnlockRouting()
			swaps++
			if swaps%8 == 0 {
				cfg.LockRouting()
				cfg.Routing = old
				cfg.UnlockRouting()
			}
		}
	}()

	// Reader: the export encoder itself, through the real
	// route (gateway-key auth + typed EXPORT), on the same
	// cfg the writers mirror into.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, adminRaw, loopbackPeer)
			if rec.Code != http.StatusOK {
				body := rec.Body.String()
				if len(body) > 200 {
					body = body[:200]
				}
				t.Errorf("db export during soak: status %d: %s", rec.Code, body)
				return
			}
		}
	}()

	// Readers: the strategies + cooldown feeds, through the
	// real dashboard routes.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			if rec := doReq(t, srv, "GET", "/dashboard/strategies", "", raw); rec.Code != http.StatusOK {
				t.Errorf("strategies feed during soak: status %d", rec.Code)
				return
			}
			if rec := doReq(t, srv, "GET", "/dashboard/cooldown", "", raw); rec.Code != http.StatusOK {
				t.Errorf("cooldown feed during soak: status %d", rec.Code)
				return
			}
		}
	}()

	// Readers: strategyForModel + Clone + the group
	// snapshots, on the same cfg the writers mutate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			for i := 0; i < writers; i++ {
				name := fmt.Sprintf("race-%d", i)
				_, _, _ = srv.strategyForModel(name)
				if members, ok := cfg.GroupMembers(name); ok && len(members) != 2 {
					t.Errorf("GroupMembers(%q) = %v, want 2 members", name, members)
					return
				}
			}
			if clone := cfg.Clone(); clone == nil || clone.Routing == nil {
				t.Errorf("Clone lost the routing block")
				return
			}
			_ = cfg.GroupsSnapshot()
			_ = srv.groupsFeed()
		}
	}()

	close(start)
	wg.Wait()

	// The store still answers after the soak, and one last
	// export is a clean 200.
	if code := do(http.MethodGet, "/v1/combos", ""); code != http.StatusOK {
		t.Fatalf("list after soak: status %d, want 200", code)
	}
	if rec := doReqPeer(t, srv, "POST", "/admin/db/export", `{"confirm": "EXPORT"}`, adminRaw, loopbackPeer); rec.Code != http.StatusOK {
		t.Fatalf("db export after soak: status %d, want 200", rec.Code)
	}
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
