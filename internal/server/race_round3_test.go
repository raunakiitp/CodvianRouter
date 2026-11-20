// M8.14 race fix, round 3: probes for the three holes the
// independent re-reviewer returned FAIL on.
//
//  1. NewComboStore's unlocked `cfg.Routing == nil` check —
//     the store is built LAZILY on the first live /v1/combos
//     request, concurrent with serving, so the read raced
//     routingPersist's swap and mirrorGroup's Routing
//     creation. TestCombosLazyNilRoutingRace pins it: the
//     config starts with a nil Routing block while a swap loop
//     flips it between nil and populated and goroutines drive
//     the lazy combosStore() construction.
//  2. Whole-struct config restores (`*s.cfg = *snapshot` /
//     `*s.cfg = *defs`) in danger.go and keystatus.go — the
//     Routing pointer (and, on reset, the live mutexes
//     themselves) were swapped without holding routingMu.
//     TestDangerKeystatusRestoreRace pins it: a FAILING
//     persist forces every rollback (RestoreFrom) on every
//     call while combo mirrors, the swap loop and Clone run.
//  3. Clone / Save range ProviderPrices without the prices
//     lock while the pricing PATCH writes it.
//     TestPricingCloneSaveRace pins it: concurrent PATCH +
//     Clone + Save traffic against a real Save.
//
// Each test must FAIL under -race with its fix reverted
// (toggle in a scratch copy — see docs/LOG.md, M8.14 race
// fix round 3) and PASS with the fix. They assert nothing
// about final state (writers end mid-cycle by design) —
// only that the run is race-free and the server still
// answers afterwards.
package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// round3SwapLoop runs the routingPersist-style pointer swap:
// copy the live block under the write lock, flip the global
// strategy, swap the pointer — alternating between a
// populated block and nil, so a reader that loads cfg.Routing
// unlocked races the write either way. Every eighth swap
// rolls the pointer back, mimicking a failed Save.
func round3SwapLoop(t *testing.T, cfg *config.Config, start <-chan struct{}, deadline time.Time, wg *sync.WaitGroup) {
	t.Helper()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		flip := false
		swaps := 0
		for time.Now().Before(deadline) {
			cfg.LockRouting()
			var next *config.RoutingConfig
			if flip {
				next = &config.RoutingConfig{
					GlobalStrategy: "balanced",
					Groups:         map[string][]string{"fast": {"a", "b"}},
				}
			}
			flip = !flip
			old := cfg.Routing
			cfg.Routing = next
			cfg.UnlockRouting()
			swaps++
			if swaps%8 == 0 {
				cfg.LockRouting()
				cfg.Routing = old
				cfg.UnlockRouting()
			}
		}
	}()
}

func TestCombosLazyNilRoutingRace(t *testing.T) {
	srv, _, _, _ := groupStack(t)
	cfg := srv.cfg
	// Single-threaded, before any goroutine starts: the first
	// combos request will construct the store over a NIL
	// routing block — the exact shape the old unlocked
	// `cfg.Routing == nil` check read.
	cfg.Routing = nil

	const writers = 4
	deadline := time.Now().Add(2 * time.Second)
	start := make(chan struct{})
	var wg sync.WaitGroup

	// Lazy construction: every goroutine drives
	// combosStore() (serialised by combosMu, but the
	// construction itself ran unlocked against the swap
	// loop pre-fix) and then lists combos through it.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for time.Now().Before(deadline) {
				store := srv.combosStore()
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodGet, "/v1/combos", nil)
				store.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("combos list during nil-routing soak: status %d", rec.Code)
					return
				}
			}
		}()
	}

	round3SwapLoop(t, cfg, start, deadline, &wg)

	// Readers: Clone + snapshots on the same cfg.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			if clone := cfg.Clone(); clone == nil {
				t.Errorf("Clone returned nil")
				return
			}
			_ = cfg.GroupsSnapshot()
		}
	}()

	close(start)
	wg.Wait()

	// The store still answers after the soak, and the nil
	// block seeded nothing (no crash, no invented combos).
	store := srv.combosStore()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/combos", nil)
	store.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list after soak: status %d, want 200", rec.Code)
	}
}

func TestDangerKeystatusRestoreRace(t *testing.T) {
	srv, _, _, _ := groupStack(t)
	cfg := srv.cfg
	// A FAILING persist forces the rollback path — i.e.
	// RestoreFrom — on every danger / keystatus call below.
	// (The persist is assigned directly: the test server
	// wires none by default, and a nil persist would make
	// the handlers answer 503 without touching the config.)
	srv.configPersist = func() error { return errors.New("round3 probe: persist boom") }

	// One key per key-mutation op, so the ops do not 404 each
	// other out of existence mid-soak (single-threaded setup).
	updKey, _, err := config.IssueKey("probe-upd", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	delKey, _, err := config.IssueKey("probe-del", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	rotKey, _, err := config.IssueKey("probe-rot", []string{"chat", "admin"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cfg.GatewayKeys = append(cfg.GatewayKeys, updKey, delKey, rotKey)

	store := NewComboStore(cfg, nil)
	h := store.Handler()
	doCombo := func(method, path, body string) int {
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
	doDanger := func(handler func(http.ResponseWriter, *http.Request), body string) int {
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}
	doKey := func(handler func(http.ResponseWriter, *http.Request), method, id, body string) int {
		req := httptest.NewRequest(method, "/", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec.Code
	}

	deadline := time.Now().Add(2 * time.Second)
	start := make(chan struct{})
	var wg sync.WaitGroup

	// The danger and key handlers are serialised against EACH
	// OTHER by opsMu — deliberately, and only for this probe:
	// concurrent danger + keystatus mutation races on
	// cfg.GatewayKeys itself (keyMu guards update/delete/
	// rotate, dangerMu guards revoke-all/reset, the bootstrap
	// issuer appends with no lock at all), which is the
	// item-4 open follow-up, NOT this round's defect. The
	// defect under test here is RestoreFrom's Routing-pointer
	// (and mutex-object) swap racing the swap loop and the
	// combo mirrors — and that race is fully exercised with
	// the handlers serialised, because the restores still run
	// concurrently with the swap loop, the mirrors and the
	// snapshot readers below. Serialising the handlers does
	// not hide it: pre-fix, the plain `*s.cfg = *snapshot`
	// write is unordered against the swap loop's locked
	// write, and the toggle proves it (see docs/LOG.md).
	var opsMu sync.Mutex
	doDangerLocked := func(handler func(http.ResponseWriter, *http.Request), body string) int {
		opsMu.Lock()
		defer opsMu.Unlock()
		return doDanger(handler, body)
	}
	doKeyLocked := func(handler func(http.ResponseWriter, *http.Request), method, id, body string) int {
		opsMu.Lock()
		defer opsMu.Unlock()
		return doKey(handler, method, id, body)
	}

	// Danger traffic: revoke-all + reset, both rolling back
	// on every call (persist always fails). 500 is the
	// expected rollback answer; anything else is a bug.
	for _, handler := range []struct {
		fn   func(http.ResponseWriter, *http.Request)
		body string
	}{
		{srv.handleRevokeAllKeys, `{"confirm":"REVOKE ALL"}`},
		{srv.handleResetConfig, `{"confirm":"RESET CONFIG"}`},
	} {
		wg.Add(1)
		go func(fn func(http.ResponseWriter, *http.Request), body string) {
			defer wg.Done()
			<-start
			for time.Now().Before(deadline) {
				if code := doDangerLocked(fn, body); code != http.StatusInternalServerError {
					t.Errorf("danger rollback call: status %d, want 500", code)
					return
				}
			}
		}(handler.fn, handler.body)
	}

	// Key traffic: update / delete / rotate, each on its own
	// key, each rolling back on every call. Update and rotate
	// answer 500 on persist failure; delete answers 500 too,
	// or 404 once its key is gone — both are honest.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			if code := doKeyLocked(srv.handleKeyUpdate, http.MethodPost, updKey.ID, `{"disabled":true}`); code != http.StatusInternalServerError {
				t.Errorf("key update rollback: status %d, want 500", code)
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			code := doKeyLocked(srv.handleKeyDelete, http.MethodDelete, delKey.ID, fmt.Sprintf(`{"confirm":"DELETE %s"}`, delKey.ID))
			if code != http.StatusInternalServerError && code != http.StatusNotFound {
				t.Errorf("key delete rollback: status %d, want 500 or 404", code)
				return
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			code := doKeyLocked(srv.handleKeyRotate, http.MethodPost, rotKey.ID, `{}`)
			if code != http.StatusInternalServerError && code != http.StatusNotFound {
				t.Errorf("key rotate rollback: status %d, want 500 or 404", code)
				return
			}
		}
	}()

	// Combo mirrors + the swap loop, racing the restores
	// above. The mirrors run DIRECTLY (mirrorGroup /
	// mirrorStrategy / unmirror under ComboStore.mu — the
	// handlers' exact lock order), not through the HTTP
	// layer: the HTTP path ranges cfg.Providers unlocked
	// (providerByID in the combo view), and that read races
	// RestoreFrom's Providers-header write — the item-4 open
	// follow-up (no guard exists for Providers), not this
	// defect. The mirror writes themselves are the traffic
	// this probe needs: they rewrite the routing maps and
	// create the Routing block, racing the restores.
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("round3-%d", w)
			steps := []comboStoredStep{{provider: "a"}, {provider: "b"}}
			for time.Now().Before(deadline) {
				store.mu.Lock()
				store.mirrorGroup(name, steps)
				store.mirrorStrategy(name, "cheapest")
				store.unmirror(name)
				store.mu.Unlock()
			}
		}(w)
	}
	round3SwapLoop(t, cfg, start, deadline, &wg)
	// Snapshot readers: GroupsSnapshot / ChainStrategy only —
	// no standalone Clone goroutine here, because Clone reads
	// GatewayKeys, which the danger/key handlers mutate
	// concurrently (the item-4 open follow-up, not this
	// defect); the handlers' own snapshot Clones still run.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			_ = cfg.GroupsSnapshot()
			_, _ = cfg.ChainStrategy("fast")
			// No groupsFeed here: it ranges cfg.Providers
			// unlocked (item-4 open follow-up, same reason
			// as the direct mirrors above).
		}
	}()

	close(start)
	wg.Wait()

	// The server still answers, and the rollback traffic left
	// a live routing block (every restore put the snapshot
	// back — the store's mirrors still have somewhere to land).
	if code := doCombo(http.MethodGet, "/v1/combos", ""); code != http.StatusOK {
		t.Fatalf("list after soak: status %d, want 200", code)
	}
	if clone := cfg.Clone(); clone == nil {
		t.Fatalf("Clone after soak returned nil")
	}
}

func TestPricingCloneSaveRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig(t.TempDir())
	cfg.Providers = []config.ProviderConfig{
		{ID: "mock-upstream", Name: "Mock Upstream", BaseURL: "http://127.0.0.1:9", WireFormat: "openai", Enabled: true,
			Models: []provider.Model{{ID: "mock-model"}}},
	}
	cfg.Routing = &config.RoutingConfig{
		Groups: map[string][]string{"fast": {"mock-upstream"}},
	}
	cfg.ProviderPrices = map[string]map[string]config.ModelPrice{
		"mock-upstream": {"mock-model": {
			InputUSDPer1M:  pricePtr(2.5),
			OutputUSDPer1M: pricePtr(10),
		}},
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	srv := newTestServer(t, &fakeEngine{},
		WithConfig(cfg),
		// A REAL persist: PATCH's configPersist must run
		// Save (Marshal under the prices read lock) against
		// the PATCH's own ProviderPrices writes.
		WithConfigPersist(func() error { return cfg.Save(path) }),
		WithRegistry(&m815FakeRegistry{models: []provider.DashboardModel{
			{ID: "mock-model", ProviderID: "mock-upstream", ProviderName: "Mock Upstream"},
		}}),
		WithPriceBook(m815PriceBook()),
	)
	srv.SetLedgerPrices(m815LedgerPrices())

	deadline := time.Now().Add(2 * time.Second)
	start := make(chan struct{})
	var wg sync.WaitGroup

	// PATCH writers: the same override every time, so the
	// final value is deterministic.
	patchBody := `{"provider":"mock-upstream","model":"mock-model","input_usd_per_1m":3.5}`
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for time.Now().Before(deadline) {
				rec := m815CallPricing(t, srv, http.MethodPatch, "/api/pricing", patchBody)
				if rec.Code != http.StatusOK {
					t.Errorf("PATCH: status %d, body %s", rec.Code, rec.Body.String())
					return
				}
			}
		}()
	}

	// Clone readers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			clone := cfg.Clone()
			if clone == nil || clone.ProviderPrices == nil {
				t.Errorf("Clone lost ProviderPrices")
				return
			}
		}
	}()

	// Save readers: a second, direct Save to the same file —
	// the configPersist path Save takes from danger/keystatus
	// and the registry store.
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			if err := cfg.Save(path); err != nil {
				t.Errorf("Save: %v", err)
				return
			}
		}
	}()

	// GET pricing readers (pricingMu path).
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for time.Now().Before(deadline) {
			rec := m815CallPricing(t, srv, http.MethodGet, "/api/pricing?provider=mock-upstream&model=mock-model", "")
			if rec.Code != http.StatusOK {
				t.Errorf("GET pricing: status %d", rec.Code)
				return
			}
		}
	}()

	close(start)
	wg.Wait()

	// The override landed (every PATCH wrote the same value).
	snap := cfg.ProviderPricesSnapshot()
	got, ok := snap["mock-upstream"]["mock-model"]
	if !ok || got.InputUSDPer1M == nil || *got.InputUSDPer1M != 3.5 {
		t.Fatalf("ProviderPrices after soak = %+v, want input 3.5", got)
	}
}
