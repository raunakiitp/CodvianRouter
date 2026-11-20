package main

// M9 merge wave (Builder C): registryMCPSource behaviour tests —
// the 500-event metrics cap (N2) and persist-or-rollback +
// lazy-ledger initialisation (N3). These tests build a real
// registryMCPSource (same package) against a real ledger, not
// the MCP fake: the cap, the rollback and the laziness all live
// in this source, so only a live ledger proves them.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/a2a"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/mcp"
)

// newTestMCPSource builds a registryMCPSource over a fresh init +
// one mock provider (fireworks, model-a), mirroring
// TestRunMCPToolsCallLiveData. The config is real, so Save
// succeeds on the returned configPath. It also returns the
// provider's registry id (a catalog add mints prov_* — the
// catalog id "fireworks" is NOT the provider id).
func newTestMCPSource(t *testing.T) (*registryMCPSource, *config.Config, string, string) {
	t.Helper()
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	mock := mockUpstream(t)
	t.Cleanup(mock.Close)
	if code := run([]string{"provider", "add-catalog", "fireworks", "fw-test-key",
		"--endpoint", "chat", "--variant", "alt-header", "--base-url", mock.URL}); code != 0 {
		t.Fatalf("provider add-catalog exit = %d, want 0", code)
	}
	reg, path, err := openRegistry(options{})
	if err != nil {
		t.Fatalf("openRegistry: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Providers) != 1 {
		t.Fatalf("config has %d providers, want 1", len(cfg.Providers))
	}
	return &registryMCPSource{reg: reg, cfg: cfg, configPath: path}, cfg, path, cfg.Providers[0].ID
}

// blockedConfigPath returns a config path whose Save always fails:
// a path component is a regular file, so MkdirAll fails. The
// config itself stays valid — only the write fails.
func blockedConfigPath(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return filepath.Join(blocker, "config.json")
}

// TestRegistryMCPSourceMetrics500EventCap (N2): ComboMetrics /
// ModelStats read at most the 500 most recent ledger events.
// Seed 100 old events at 10000 ms latency, then 500 newer events
// at 100 ms: the latency mean must come from the 500 newer events
// only (100 ms), ModelStats must count 500 requests (not 600),
// and ComboMetrics' all-time counts must still be 600.
func TestRegistryMCPSourceMetrics500EventCap(t *testing.T) {
	src, _, _, providerID := newTestMCPSource(t)
	if err := src.CreateCombo(context.Background(), "solo", []string{providerID}, "priority"); err != nil {
		t.Fatalf("CreateCombo: %v", err)
	}
	led, err := src.openLedger(true)
	if err != nil {
		t.Fatalf("openLedger(true): %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		if _, err := led.RecordShared(ctx, providerID, "model-a", true, 10000); err != nil {
			t.Fatalf("seed old event %d: %v", i, err)
		}
	}
	for i := 0; i < 500; i++ {
		if _, err := led.RecordShared(ctx, providerID, "model-a", true, 100); err != nil {
			t.Fatalf("seed recent event %d: %v", i, err)
		}
	}

	stats := src.ModelStats()
	if len(stats) != 1 {
		t.Fatalf("ModelStats returned %d rows, want 1 (%s/model-a)", len(stats), providerID)
	}
	st := stats[0]
	if st.Provider != providerID || st.Model != "model-a" {
		t.Fatalf("ModelStats row = %+v, want %s/model-a", st, providerID)
	}
	if st.Requests != 500 || st.OK != 500 {
		t.Errorf("ModelStats requests/ok = %d/%d, want 500/500 (the 100 older events are dropped by the cap)", st.Requests, st.OK)
	}
	if st.LatencyEvents != 500 || st.MeanLatencyMS != 100 || !st.LatencyKnown {
		t.Errorf("ModelStats latency = %d events / %.1f ms / known=%v, want 500 / 100 / true (old 10000 ms events excluded)", st.LatencyEvents, st.MeanLatencyMS, st.LatencyKnown)
	}

	m, err := src.ComboMetrics("solo")
	if err != nil {
		t.Fatalf("ComboMetrics: %v", err)
	}
	// Counts/cost are all-time; latency is the capped window.
	if m.Requests != 600 || m.OK != 600 {
		t.Errorf("ComboMetrics requests/ok = %d/%d, want 600/600 (all-time)", m.Requests, m.OK)
	}
	if m.LatencyEvents != 500 || m.MeanLatencyMS != 100 || !m.LatencyKnown {
		t.Errorf("ComboMetrics latency = %d events / %.1f ms / known=%v, want 500 / 100 / true (old 10000 ms events excluded)", m.LatencyEvents, m.MeanLatencyMS, m.LatencyKnown)
	}
}

// TestRegistryMCPSourceCreateComboRollbackOnSaveFailure (N3a):
// a persist failure rolls the in-memory combo creation back —
// neither the group nor the chain-strategy mirror survives.
func TestRegistryMCPSourceCreateComboRollbackOnSaveFailure(t *testing.T) {
	src, cfg, _, providerID := newTestMCPSource(t)
	src.configPath = blockedConfigPath(t)
	err := src.CreateCombo(context.Background(), "newcombo", []string{providerID}, "priority")
	if err == nil {
		t.Fatal("CreateCombo with an unwritable config path succeeded, want a save error")
	}
	if !strings.Contains(err.Error(), "could not save config") {
		t.Fatalf("CreateCombo error = %v, want a could-not-save-config error", err)
	}
	if _, exists := cfg.GroupsSnapshot()["newcombo"]; exists {
		t.Error("combo group still present after failed save — rollback did not run")
	}
	if _, ok := cfg.ChainStrategy("newcombo"); ok {
		t.Error("chain-strategy mirror still present after failed save — rollback did not run")
	}
}

// TestRegistryMCPSourceCreateComboCancelledContext (CreateCombo
// review nit 1): the doctor's CreateCombo takes the caller's
// context, and a cancelled context aborts before any validation
// or mutation — no group, no chain-strategy mirror, and the
// config file on disk is untouched. A live context afterwards
// still creates normally, so the aborted call corrupted
// nothing.
func TestRegistryMCPSourceCreateComboCancelledContext(t *testing.T) {
	src, cfg, path, providerID := newTestMCPSource(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller went away before the create ran
	err := src.CreateCombo(ctx, "cancelled", []string{providerID}, "priority")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled CreateCombo error = %v, want context.Canceled", err)
	}
	if _, exists := cfg.GroupsSnapshot()["cancelled"]; exists {
		t.Error("cancelled CreateCombo stored a group — the aborted create persisted it")
	}
	if _, ok := cfg.ChainStrategy("cancelled"); ok {
		t.Error("cancelled CreateCombo stored a chain-strategy mirror — the aborted create persisted it")
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after cancelled create: %v", err)
	}
	if _, exists := reloaded.GroupsSnapshot()["cancelled"]; exists {
		t.Error("reloaded config contains the cancelled combo — the aborted create reached the file")
	}
	// The source is uncorrupted: a live context creates.
	if err := src.CreateCombo(context.Background(), "live", []string{providerID}, "priority"); err != nil {
		t.Fatalf("live CreateCombo after a cancelled one: %v", err)
	}
	if _, exists := cfg.GroupsSnapshot()["live"]; !exists {
		t.Error("live CreateCombo after a cancelled one stored nothing")
	}
}

// TestRegistryMCPSourceCreateComboConverged (CreateCombo review
// nit 2): the doctor's registryMCPSource.CreateCombo converges
// on ComboStore.Create via serverMCPSource.CreateCombo — same
// validation order (name, strategy, chain, duplicate), same
// error texts wrapped in mcp.ErrInvalidInput, same group-mirror
// store semantics (members stored as "provider[@effort]", the
// model pin validated but dropped, exactly like the server's
// mirrorGroup). Each error case asserts the exact message the
// server path produces for the same input (see
// TestComboStoreCreateValidation in internal/server).
func TestRegistryMCPSourceCreateComboConverged(t *testing.T) {
	src, cfg, path, providerID := newTestMCPSource(t)
	// The fixture provider declares model-a (the mock /models
	// endpoint serves it and add-catalog auto-fetches it).
	hasModelA := false
	for _, m := range cfg.Providers[0].Models {
		if m.ID == "model-a" {
			hasModelA = true
		}
	}
	if !hasModelA {
		t.Fatalf("fixture provider %s declares models %v, want model-a present (mock /models serves it)", providerID, cfg.Providers[0].Models)
	}

	// Valid create, strategy alias normalised to canonical.
	if err := src.CreateCombo(context.Background(), "conv", []string{providerID}, "cheap"); err != nil {
		t.Fatalf("CreateCombo valid: %v", err)
	}
	if got := cfg.GroupsSnapshot()["conv"]; len(got) != 1 || got[0] != providerID {
		t.Fatalf("stored group conv = %v, want [%s]", got, providerID)
	}
	if st, ok := cfg.ChainStrategy("conv"); !ok || st != "cheapest" {
		t.Fatalf("chain strategy conv = %q (present %v), want cheapest (alias cheap normalised)", st, ok)
	}

	// Empty strategy inherits "priority", like ComboStore.create.
	if err := src.CreateCombo(context.Background(), "conv-default", []string{providerID}, ""); err != nil {
		t.Fatalf("CreateCombo empty strategy: %v", err)
	}
	if st, ok := cfg.ChainStrategy("conv-default"); !ok || st != "priority" {
		t.Fatalf("chain strategy conv-default = %q (present %v), want priority", st, ok)
	}

	// Execution-mode strategy is accepted and record-only: no
	// chain-strategy mirror entry (mirrors mirrorStrategy).
	if err := src.CreateCombo(context.Background(), "conv-fusion", []string{providerID}, "fusion"); err != nil {
		t.Fatalf("CreateCombo fusion strategy: %v", err)
	}
	if st, ok := cfg.ChainStrategy("conv-fusion"); ok {
		t.Fatalf("chain strategy conv-fusion = %q, want no mirror entry for an execution mode", st)
	}

	// Model pin + effort are validated, then stored in group
	// syntax (pin dropped, effort kept) — the server mirror's
	// form: mirrorGroup writes "provider[@effort]".
	if err := src.CreateCombo(context.Background(), "conv-pin", []string{providerID + "/model-a@low"}, "priority"); err != nil {
		t.Fatalf("CreateCombo model pin: %v", err)
	}
	if got := cfg.GroupsSnapshot()["conv-pin"]; len(got) != 1 || got[0] != providerID+"@low" {
		t.Fatalf("stored group conv-pin = %v, want [%s@low] (pin dropped, effort kept)", got, providerID)
	}

	// Effort is normalised to lowercase on store.
	if err := src.CreateCombo(context.Background(), "conv-effort", []string{providerID + "@LOW"}, "priority"); err != nil {
		t.Fatalf("CreateCombo uppercase effort: %v", err)
	}
	if got := cfg.GroupsSnapshot()["conv-effort"]; len(got) != 1 || got[0] != providerID+"@low" {
		t.Fatalf("stored group conv-effort = %v, want [%s@low] (effort normalised)", got, providerID)
	}

	// Whitespace is trimmed on the name and the strategy.
	if err := src.CreateCombo(context.Background(), "  conv-trim  ", []string{providerID}, "  fastest  "); err != nil {
		t.Fatalf("CreateCombo trimmed name/strategy: %v", err)
	}
	if _, exists := cfg.GroupsSnapshot()["conv-trim"]; !exists {
		t.Fatalf("combo stored under an untrimmed name — groups: %v", cfg.GroupsSnapshot())
	}
	if st, ok := cfg.ChainStrategy("conv-trim"); !ok || st != "fastest" {
		t.Fatalf("chain strategy conv-trim = %q (present %v), want fastest", st, ok)
	}

	// The stored combos reload cleanly: Save validated the
	// groups on every create, and a fresh load sees the same
	// mirrors — nothing stored can corrupt the config.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after converged creates: %v", err)
	}
	for _, name := range []string{"conv", "conv-default", "conv-fusion", "conv-pin", "conv-effort", "conv-trim"} {
		if _, exists := reloaded.GroupsSnapshot()[name]; !exists {
			t.Errorf("reloaded config is missing combo %q", name)
		}
	}

	// Invalid inputs: exact server texts, wrapped in
	// mcp.ErrInvalidInput. A bad strategy beats a bad chain
	// (strategy is validated first, like ComboStore.create);
	// a bad chain beats a duplicate name (chain first, like
	// createLocked). Nothing is stored for any rejected input.
	errCases := []struct {
		name     string
		combo    string
		members  []string
		strategy string
		want     string
	}{
		{"empty name", "", []string{providerID}, "priority", "mcp: invalid input: name: must not be empty"},
		{"slash name", "a/b", []string{providerID}, "priority", `mcp: invalid input: name: "a/b" must not contain ":" or "/" (routing group names use "group:<name>/<model>" alias syntax)`},
		{"colon name", "a:b", []string{providerID}, "priority", `mcp: invalid input: name: "a:b" must not contain ":" or "/" (routing group names use "group:<name>/<model>" alias syntax)`},
		{"unknown strategy", "ok", []string{providerID}, "nope", `mcp: invalid input: strategy: unknown strategy "nope"`},
		{"unknown strategy trimmed", "ok", []string{providerID}, "  nope  ", `mcp: invalid input: strategy: unknown strategy "nope"`},
		{"strategy beats chain", "ok", []string{"ghost"}, "nope", `mcp: invalid input: strategy: unknown strategy "nope"`},
		{"empty chain", "ok", nil, "priority", "mcp: invalid input: chain: must not be empty (a combo needs at least one provider)"},
		{"empty member", "ok", []string{""}, "priority", "mcp: invalid input: chain[0]: must not be empty"},
		{"whitespace member", "ok", []string{providerID, "   "}, "priority", "mcp: invalid input: chain[1]: must not be empty"},
		{"unknown provider", "ok", []string{"ghost"}, "priority", `mcp: invalid input: chain[0].provider: unknown provider "ghost"`},
		{"unknown model", "ok", []string{providerID + "/nope"}, "priority", `mcp: invalid input: chain[0].model: unknown model "nope" for provider "` + providerID + `"`},
		{"unknown effort", "ok", []string{providerID + "@bogus"}, "priority", `mcp: invalid input: chain[0].effort: unknown reasoning effort "bogus" (one of none, minimal, low, medium, high, xhigh)`},
		{"empty provider", "ok", []string{"/model-a"}, "priority", "mcp: invalid input: chain[0].provider: must not be empty"},
		{"effort without provider", "ok", []string{"@low"}, "priority", "mcp: invalid input: chain[0].provider: must not be empty"},
		{"duplicate step", "ok", []string{providerID, providerID}, "priority", `mcp: invalid input: chain[1]: duplicate step "` + providerID + `" (same provider, model and effort as chain[0])`},
		{"chain beats duplicate", "conv", []string{"ghost"}, "priority", `mcp: invalid input: chain[0].provider: unknown provider "ghost"`},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			err := src.CreateCombo(context.Background(), tc.combo, tc.members, tc.strategy)
			if err == nil {
				t.Fatalf("CreateCombo(%q) succeeded, want an error", tc.name)
			}
			if !errors.Is(err, mcp.ErrInvalidInput) {
				t.Fatalf("CreateCombo(%q) error = %v, want it to wrap mcp.ErrInvalidInput", tc.name, err)
			}
			if err.Error() != tc.want {
				t.Fatalf("CreateCombo(%q) error = %q, want exactly %q", tc.name, err.Error(), tc.want)
			}
		})
	}
	if _, exists := cfg.GroupsSnapshot()["ok"]; exists {
		t.Error("rejected creates stored a group named ok — nothing may be stored for a denied call")
	}

	// Duplicate name: same text as the server's 409, the first
	// combo untouched even when the retry carries a valid but
	// different chain.
	if err := src.CreateCombo(context.Background(), "conv", []string{providerID + "@low"}, "priority"); err == nil {
		t.Fatal("duplicate CreateCombo succeeded, want a conflict error")
	} else if !errors.Is(err, mcp.ErrInvalidInput) || err.Error() != `mcp: invalid input: name: combo "conv" already exists` {
		t.Fatalf("duplicate CreateCombo error = %v, want exactly %q", err, `mcp: invalid input: name: combo "conv" already exists`)
	}
	if got := cfg.GroupsSnapshot()["conv"]; len(got) != 1 || got[0] != providerID {
		t.Fatalf("group conv after duplicate attempt = %v, want [%s] (first combo untouched)", got, providerID)
	}
	if st, ok := cfg.ChainStrategy("conv"); !ok || st != "cheapest" {
		t.Fatalf("chain strategy conv after duplicate attempt = %q (present %v), want cheapest (untouched)", st, ok)
	}
}

// TestRegistryMCPSourceCreateComboConcurrentDuplicate (CreateCombo
// review nit 2, TOCTOU): the duplicate check runs UNDER the routing
// write lock, so the check and the insert are one atomic step.
// 16 goroutines creating the same name at once must yield exactly
// one success and 15 duplicate errors, and the stored group must
// be exactly one goroutine's chain — never a mix, and never a
// silent overwrite. With the old pre-lock check, two or more
// goroutines could pass the check before either inserted and all
// would report success, the last writer silently winning. The
// reloaded config must also hold the winning group: concurrent
// Saves are atomic renames, so the file stays valid.
func TestRegistryMCPSourceCreateComboConcurrentDuplicate(t *testing.T) {
	src, cfg, path, providerID := newTestMCPSource(t)
	variants := [][]string{
		{providerID},
		{providerID + "@low"},
		{providerID, providerID + "@low"},
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = src.CreateCombo(context.Background(), "race", variants[i%len(variants)], "priority")
		}(i)
	}
	wg.Wait()
	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		if !errors.Is(err, mcp.ErrInvalidInput) || err.Error() != `mcp: invalid input: name: combo "race" already exists` {
			t.Errorf("goroutine %d error = %v, want the exact duplicate error", i, err)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d concurrent creates succeeded, want exactly 1 (the rest must be duplicate errors)", succeeded)
	}
	got := cfg.GroupsSnapshot()["race"]
	matched := false
	for _, want := range variants {
		if len(got) == len(want) {
			same := true
			for j := range want {
				if got[j] != want[j] {
					same = false
				}
			}
			if same {
				matched = true
			}
		}
	}
	if !matched {
		t.Fatalf("stored group race = %v, want exactly one of %v (one winner's chain, not a mix)", got, variants)
	}
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after concurrent creates: %v", err)
	}
	reloadedGroups := reloaded.GroupsSnapshot()["race"]
	if len(reloadedGroups) != len(got) {
		t.Fatalf("reloaded group race = %v, want %v (the winning chain persisted)", reloadedGroups, got)
	}
	for j := range got {
		if reloadedGroups[j] != got[j] {
			t.Fatalf("reloaded group race = %v, want %v (the winning chain persisted)", reloadedGroups, got)
		}
	}
}

// TestRegistryMCPSourceSetRoutingStrategyRollbackOnSaveFailure
// (N3a): a persist failure rolls the in-memory strategy change
// back, for both the global strategy and a chain override.
func TestRegistryMCPSourceSetRoutingStrategyRollbackOnSaveFailure(t *testing.T) {
	src, cfg, _, _ := newTestMCPSource(t)
	// Good-path pre-state, persisted: global cheapest.
	if err := src.SetRoutingStrategy("", "cheapest"); err != nil {
		t.Fatalf("SetRoutingStrategy (good path): %v", err)
	}
	if got := src.RoutingStrategy("").Name; got != "cheapest" {
		t.Fatalf("global strategy after good-path set = %q, want cheapest", got)
	}
	src.configPath = blockedConfigPath(t)
	if err := src.SetRoutingStrategy("", "fastest"); err == nil {
		t.Fatal("SetRoutingStrategy with an unwritable config path succeeded, want a save error")
	}
	if got := src.RoutingStrategy("").Name; got != "cheapest" {
		t.Errorf("global strategy after failed save = %q, want cheapest (rolled back)", got)
	}
	if err := src.SetRoutingStrategy("model-a", "fastest"); err == nil {
		t.Fatal("chain SetRoutingStrategy with an unwritable config path succeeded, want a save error")
	}
	if _, ok := cfg.ChainStrategy("model-a"); ok {
		t.Error("chain strategy present after failed save — rollback did not run")
	}
}

// newLiteralMCPSource builds a registryMCPSource over a literal
// config (no init flow): a data dir plus one combo. Only for the
// lazy-ledger tests, which need a data dir with no ledger yet.
func newLiteralMCPSource(t *testing.T, dataDir string) *registryMCPSource {
	t.Helper()
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Listen:  "127.0.0.1:20128",
		DataDir: dataDir,
		Routing: &config.RoutingConfig{Groups: map[string][]string{"solo": {"p1"}}},
	}
	cfg.EnsureRoutingMu()
	cfg.EnsurePricesMu()
	return &registryMCPSource{cfg: cfg, configPath: filepath.Join(t.TempDir(), "config.json")}
}

// TestRegistryMCPSourceLazyLedgerReadDoesNotCreate (N3b): read
// tools never create the ledger file — a missing ledger is an
// honest "no ledger yet" error, and the file stays absent.
func TestRegistryMCPSourceLazyLedgerReadDoesNotCreate(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	ledPath := filepath.Join(dir, "ledger.db")
	if _, err := src.ComboMetrics("solo"); err == nil {
		t.Fatal("ComboMetrics with no ledger succeeded, want unavailable")
	} else if !strings.Contains(err.Error(), "no ledger yet") {
		t.Errorf("ComboMetrics error = %v, want a no-ledger-yet error", err)
	}
	if stats := src.ModelStats(); len(stats) != 0 {
		t.Errorf("ModelStats with no ledger = %v, want empty", stats)
	}
	if _, err := os.Stat(ledPath); !os.IsNotExist(err) {
		t.Errorf("ledger.db exists after read-only calls (stat err=%v) — a read created the database", err)
	}
}

// TestRegistryMCPSourceLazyLedgerStickyError (N3b): the first
// open outcome sticks, including the error — a ledger file that
// appears afterwards does not become visible to the same source,
// while a fresh source sees it fine.
func TestRegistryMCPSourceLazyLedgerStickyError(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	if _, err := src.ComboMetrics("solo"); err == nil {
		t.Fatal("ComboMetrics with no ledger succeeded, want unavailable")
	}
	led, err := ledger.Open(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("ledger.Open after the failed read: %v", err)
	}
	if err := led.Close(); err != nil {
		t.Fatalf("ledger.Close: %v", err)
	}
	if _, err := src.ComboMetrics("solo"); err == nil {
		t.Error("ComboMetrics succeeded after the ledger appeared — the sticky open error did not stick")
	}
	fresh := newLiteralMCPSource(t, dir)
	// The fresh source caches the ledger handle its ComboMetrics call
	// opened; close it before the test ends so the TempDir cleanup can
	// remove ledger.db — on Windows an open SQLite file cannot be
	// deleted. (registryMCPSource has no Close: in the stdio server
	// the handle lives until process exit, which is fine there.)
	t.Cleanup(func() {
		fresh.ledMu.Lock()
		defer fresh.ledMu.Unlock()
		if fresh.led != nil {
			_ = fresh.led.Close()
		}
	})
	if _, err := fresh.ComboMetrics("solo"); err != nil {
		t.Errorf("fresh source ComboMetrics = %v, want nil error (empty ledger)", err)
	}
}

// TestRegistryMCPSourceLazyLedgerCreateOnWrite (N3b): only a
// write path opens-or-creates the ledger, and the handle is
// opened once and cached — a later read returns the same one.
func TestRegistryMCPSourceLazyLedgerCreateOnWrite(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	ledPath := filepath.Join(dir, "ledger.db")
	led, err := src.openLedger(true)
	if err != nil {
		t.Fatalf("openLedger(true): %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	if _, err := os.Stat(ledPath); err != nil {
		t.Errorf("ledger.db missing after openLedger(true): %v", err)
	}
	again, err := src.openLedger(false)
	if err != nil {
		t.Fatalf("openLedger(false) after create: %v", err)
	}
	if again != led {
		t.Error("openLedger(false) returned a different handle — the ledger is not opened once and cached")
	}
	if _, err := src.ComboMetrics("solo"); err != nil {
		t.Errorf("ComboMetrics after create: %v, want nil", err)
	}
}

// callMCPTool calls one tool in-process over the stdio source —
// the same mcp.Server the mcp command builds (LocalOperator
// identity, same as stdio) — and returns the decoded JSON-RPC
// response envelope.
func callMCPTool(t *testing.T, src *registryMCPSource, name string, args map[string]any) map[string]any {
	t.Helper()
	srv := mcp.New(src, Version).WithClientIdentity(mcp.LocalOperator())
	rawArgs, _ := json.Marshal(args)
	msg, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": json.RawMessage(rawArgs)},
	})
	raw := srv.Handle(context.Background(), msg)
	if raw == nil {
		t.Fatalf("Handle(tools/call %s) returned nil, want a response", name)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	return out
}

// toolErrMsg extracts the JSON-RPC error code/message from a
// tools/call response envelope: (0, "") when the call succeeded.
func toolErrMsg(t *testing.T, out map[string]any) (float64, string) {
	t.Helper()
	errBody, ok := out["error"].(map[string]any)
	if !ok {
		return 0, ""
	}
	code, _ := errBody["code"].(float64)
	msg, _ := errBody["message"].(string)
	return code, msg
}

// toolResultModel extracts the picked model from a successful
// pick_fastest_model response (structuredContent.model).
func toolResultModel(t *testing.T, out map[string]any) string {
	t.Helper()
	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call result missing: %v", out)
	}
	sc, ok := result["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("structuredContent missing: %v", result)
	}
	model, _ := sc["model"].(string)
	return model
}

// TestRegistryMCPSourceModelStatsAvailableCorruptLedger (M9.2 N1
// follow-up): with a corrupt/unreadable ledger.db, the stdio
// source reports the latency backend unavailable —
// pick_fastest_model says "backend is not available", not "no
// measured model latency yet" — and cost_report on the same
// source agrees the ledger is unavailable.
func TestRegistryMCPSourceModelStatsAvailableCorruptLedger(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	ledPath := filepath.Join(dir, "ledger.db")
	if err := os.WriteFile(ledPath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}
	if src.ModelStatsAvailable() {
		t.Error("ModelStatsAvailable() = true with a corrupt ledger, want false (backend unavailable)")
	}
	if stats := src.ModelStats(); len(stats) != 0 {
		t.Errorf("ModelStats with a corrupt ledger = %v, want empty", stats)
	}
	code, msg := toolErrMsg(t, callMCPTool(t, src, "pick_fastest_model", map[string]any{}))
	if code != -32602 || !strings.Contains(msg, "backend is not available") {
		t.Errorf("corrupt-ledger pick_fastest_model code/msg = %v / %q, want -32602 saying the backend is not available", code, msg)
	}
	if strings.Contains(msg, "no measured model latency yet") {
		t.Errorf("corrupt-ledger pick_fastest_model still says %q — the old message must not survive a corrupt ledger", msg)
	}
	// Consistency: cost_report on the same source must say the
	// ledger is unavailable too — both tools read the same
	// openLedger failure.
	code, costMsg := toolErrMsg(t, callMCPTool(t, src, "cost_report", map[string]any{}))
	if code != -32602 || !strings.Contains(costMsg, "unavailable") {
		t.Errorf("corrupt-ledger cost_report code/msg = %v / %q, want -32602 saying unavailable", code, costMsg)
	}
	t.Logf("corrupt ledger: pick_fastest_model = %q; cost_report = %q", msg, costMsg)
}

// TestRegistryMCPSourceModelStatsAvailableMissingLedger (M9.2 N1
// follow-up): with no ledger file yet, the backend is available
// — there simply are no events — so pick_fastest_model keeps the
// old "no measured model latency yet" message, and the check
// itself never creates the ledger file.
func TestRegistryMCPSourceModelStatsAvailableMissingLedger(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	ledPath := filepath.Join(dir, "ledger.db")
	if !src.ModelStatsAvailable() {
		t.Error("ModelStatsAvailable() = false with no ledger yet, want true (no events is not a backend failure)")
	}
	code, msg := toolErrMsg(t, callMCPTool(t, src, "pick_fastest_model", map[string]any{}))
	if code != -32602 || !strings.Contains(msg, "no measured model latency yet") || strings.Contains(msg, "backend is not available") {
		t.Errorf("missing-ledger pick_fastest_model code/msg = %v / %q, want -32602 saying no measured latency yet (not backend unavailable)", code, msg)
	}
	if _, err := os.Stat(ledPath); !os.IsNotExist(err) {
		t.Errorf("ledger.db exists after the missing-ledger calls (stat err=%v) — the availability check created the database", err)
	}
}

// TestRegistryMCPSourceModelStatsAvailableHealthyLedger (M9.2 N1
// follow-up): with a readable ledger holding measured latency,
// the backend is available and pick_fastest_model ranks on it
// normally — the fastest model wins.
func TestRegistryMCPSourceModelStatsAvailableHealthyLedger(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	led, err := src.openLedger(true)
	if err != nil {
		t.Fatalf("openLedger(true): %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	ctx := context.Background()
	if _, err := led.RecordShared(ctx, "p1", "model-fast", true, 50); err != nil {
		t.Fatalf("seed fast event: %v", err)
	}
	if _, err := led.RecordShared(ctx, "p1", "model-slow", true, 500); err != nil {
		t.Fatalf("seed slow event: %v", err)
	}
	if !src.ModelStatsAvailable() {
		t.Error("ModelStatsAvailable() = false with a healthy ledger, want true")
	}
	out := callMCPTool(t, src, "pick_fastest_model", map[string]any{})
	if code, msg := toolErrMsg(t, out); code != 0 {
		t.Fatalf("healthy-ledger pick_fastest_model failed: code %v msg %q", code, msg)
	}
	if got := toolResultModel(t, out); got != "model-fast" {
		t.Errorf("pick_fastest_model picked %q, want model-fast (50 ms beats 500 ms)", got)
	}
}

// TestRegistryMCPSourceModelStatsAvailableNoDataDir: with no data
// dir configured, openLedger fails for a reason other than a
// missing file, so the backend is unavailable.
func TestRegistryMCPSourceModelStatsAvailableNoDataDir(t *testing.T) {
	src := &registryMCPSource{cfg: &config.Config{Version: config.CurrentVersion}}
	if src.ModelStatsAvailable() {
		t.Error("ModelStatsAvailable() = true with no data dir configured, want false")
	}
}

// --- H4 review nits (LOW) -------------------------------------------------

// TestRegistryMCPSourceInspectDBsScrubbedCountError (nit 1): with
// a corrupt/unreadable ledger.db, inspectDBs (via DBHealth)
// reports the fixed string "count <table>: unavailable" — never
// the sqlite driver's message ("file is not a database") and
// never the data-dir path. Defense-in-depth: the pinned
// modernc.org/sqlite driver's errors carry no path, so this
// asserts the scrubbed shape, not a live-leak reproduction.
// The open-error branch ("database unavailable") uses the same
// fixed-string pattern and is covered by inspection — sql.Open
// is lazy, so no filesystem fixture makes it fail.
func TestRegistryMCPSourceInspectDBsScrubbedCountError(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "ledger.db"), []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}
	h := src.DBHealth()
	if h.Healthy {
		t.Error("DBHealth().Healthy = true with a corrupt ledger, want false")
	}
	if len(h.Files) == 0 {
		t.Fatal("DBHealth().Files is empty, want the ledger entry")
	}
	f := h.Files[0]
	if f.Name != "ledger.db" {
		t.Fatalf("first DBHealth file = %q, want ledger.db", f.Name)
	}
	if f.Error != "count usage_events: unavailable" {
		t.Errorf("ledger Error = %q, want %q", f.Error, "count usage_events: unavailable")
	}
	if strings.Contains(f.Error, dir) {
		t.Errorf("ledger Error %q embeds the data-dir path %q — the path must never reach the caller", f.Error, dir)
	}
	if strings.Contains(f.Error, "file is not a database") {
		t.Errorf("ledger Error %q embeds the driver's message — the caller must only see the fixed string", f.Error)
	}
	// DBStats reads the same inspectDBs seam and must agree.
	st := src.DBStats()
	if len(st.Files) == 0 || st.Files[0].Error != "count usage_events: unavailable" {
		t.Errorf("DBStats ledger Error = %q, want %q", st.Files[0].Error, "count usage_events: unavailable")
	}
}

// TestRegistryMCPSourceCacheStatsScrubbedNote (nit 2): with a
// corrupt persistent cache file, CacheStats reports a fixed note
// (basename + fixed reason) — never the driver's verbatim error
// and never the full path. The open-failure branch uses the same
// fixed-string pattern ("could not be opened read-only"); it is
// not directly reachable from a filesystem fixture because
// sql.Open is lazy, so only the count branch is exercised here.
func TestRegistryMCPSourceCacheStatsScrubbedNote(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	garbage := filepath.Join(dir, "cache.db")
	if err := os.WriteFile(garbage, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("write corrupt cache file: %v", err)
	}
	src.cfg.Cache = &config.CacheConfig{ExactPath: garbage}
	st := src.CacheStats()
	if st.Persistent.Present {
		t.Error("CacheStats.Persistent.Present = true with a corrupt cache file, want false")
	}
	want := `persistent cache file "cache.db" could not be counted`
	if st.Note != want {
		t.Errorf("CacheStats Note = %q, want %q", st.Note, want)
	}
	if strings.Contains(st.Note, dir) {
		t.Errorf("CacheStats Note %q embeds the full cache path — only the basename may appear", st.Note)
	}
	if strings.Contains(st.Note, "file is not a database") {
		t.Errorf("CacheStats Note %q embeds the driver's verbatim error", st.Note)
	}
}

// TestStripCachePathError (nit 3): *SyscallError unwraps to the
// bare errno, like stripPathError in internal/mcp — plus the
// pre-existing *PathError / *LinkError behaviour as regression
// cover. A wrapped error (fmt.Errorf %w) unwraps the same way.
func TestStripCachePathError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"syscall error", &os.SyscallError{Syscall: "open", Err: syscall.ENOENT}, syscall.ENOENT},
		{"path error", &os.PathError{Op: "stat", Path: "/secret/data/cache.db", Err: syscall.EACCES}, syscall.EACCES},
		{"link error", &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: syscall.EXDEV}, syscall.EXDEV},
		{"wrapped syscall error", fmt.Errorf("wrap: %w", &os.SyscallError{Syscall: "read", Err: syscall.EIO}), syscall.EIO},
		// H4 leftover 1: a *PathError wrapping a *SyscallError must
		// still collapse to the bare errno — before the fix this
		// returned the *SyscallError ("open: no such file or
		// directory"), not syscall.ENOENT.
		{"path error wrapping syscall error", &os.PathError{Op: "open", Path: "/secret/data/cache.db", Err: &os.SyscallError{Syscall: "open", Err: syscall.ENOENT}}, syscall.ENOENT},
		{"link error wrapping syscall error", &os.LinkError{Op: "rename", Old: "/secret/a", New: "/secret/b", Err: &os.SyscallError{Syscall: "rename", Err: syscall.EXDEV}}, syscall.EXDEV},
		{"syscall error wrapping path error", &os.SyscallError{Syscall: "open", Err: &os.PathError{Op: "open", Path: "/secret/x", Err: syscall.EACCES}}, syscall.EACCES},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripCachePathError(tc.err); got != tc.want {
				t.Errorf("stripCachePathError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
	// A plain error passes through untouched.
	if err := errors.New("boom"); stripCachePathError(err) != err {
		t.Error("stripCachePathError(plain error) did not pass the error through untouched")
	}
}

// TestCacheFlushStatErrorWording (nit 4): the CacheFlush Stat
// failure is worded by errno — only a missing file reads "not
// present"; permission-denied reads "not accessible" and other
// failures read "unavailable". The error carries only the
// basename and the stripped errno, never the full path. A fake
// Stat error is used because the suite runs as root, for whom a
// chmod-000 fixture would not produce EACCES.
func TestCacheFlushStatErrorWording(t *testing.T) {
	secret := "/secret/data-dir/cache.db"
	cases := []struct {
		name       string
		err        error
		wantPhrase string
		wantErrno  string
		// Windows spellings: Go reports ENOENT as "The system
		// cannot find the file specified.", EACCES as the Unix
		// string "permission denied", and collapses ENOTDIR
		// into the not-exist family (errors.Is(..., os.ErrNotExist)
		// is true, message "The system cannot find the path
		// specified."), so the "other" case reads "not present"
		// there instead of "unavailable".
		wantPhraseWindows string
		wantErrnoWindows  string
	}{
		{"not exist", &os.PathError{Op: "stat", Path: secret, Err: syscall.ENOENT}, "not present", "no such file or directory", "not present", "The system cannot find the file specified."},
		{"permission denied", &os.PathError{Op: "stat", Path: secret, Err: syscall.EACCES}, "not accessible", "permission denied", "not accessible", "permission denied"},
		{"other", &os.PathError{Op: "stat", Path: secret, Err: syscall.ENOTDIR}, "unavailable", "not a directory", "not present", "The system cannot find the path specified."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheFlushStatError(secret, tc.err).Error()
			wantPhrase, wantErrno := tc.wantPhrase, tc.wantErrno
			if runtime.GOOS == "windows" {
				wantPhrase, wantErrno = tc.wantPhraseWindows, tc.wantErrnoWindows
			}
			if !strings.Contains(got, wantPhrase) {
				t.Errorf("cacheFlushStatError = %q, want it to say %q", got, wantPhrase)
			}
			if !strings.Contains(got, wantErrno) {
				t.Errorf("cacheFlushStatError = %q, want the errno %q", got, wantErrno)
			}
			if !strings.Contains(got, `"cache.db"`) {
				t.Errorf("cacheFlushStatError = %q, want the basename \"cache.db\"", got)
			}
			if strings.Contains(got, secret) {
				t.Errorf("cacheFlushStatError = %q, embeds the full path — only the basename may appear", got)
			}
		})
	}
	// The two failure kinds must be worded differently.
	missing := cacheFlushStatError(secret, &os.PathError{Op: "stat", Path: secret, Err: syscall.ENOENT}).Error()
	denied := cacheFlushStatError(secret, &os.PathError{Op: "stat", Path: secret, Err: syscall.EACCES}).Error()
	if strings.Contains(denied, "not present") {
		t.Errorf("permission-denied error %q still reads \"not present\" — it must read \"not accessible\"", denied)
	}
	if missing == denied {
		t.Errorf("missing-file and permission-denied errors are identical (%q) — the wording must differ by errno", missing)
	}
}

// TestRegistryMCPSourceCacheFlushMissingFile (nit 5): end to end
// through the real registry CacheFlush — a missing cache file
// yields a caller-facing error with the basename and the errno,
// never the full path.
// TestRegistryMCPSourceCacheFlushOpenErrorIsPathFree (H4 leftover
// 2, verified no-op): end to end through the real registry
// CacheFlush — when os.Stat succeeds but the open fails (a
// directory in place of the cache file, a corrupt file), the
// caller-facing error carries only the sqlite driver's message,
// never the full cache path. The directory case surfaces
// "unable to open database file: out of memory (14)" and the
// corrupt case "file is not a database (26)"; neither embeds
// the path, and no partial flush runs (the open failed before
// any FlushTenant call).
func TestRegistryMCPSourceCacheFlushOpenErrorIsPathFree(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{"directory in place of cache file", func(t *testing.T) string {
			p := filepath.Join(dir, "cache-dir.db")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatalf("mkdir cache dir fixture: %v", err)
			}
			return p
		}},
		{"corrupt cache file", func(t *testing.T) string {
			p := filepath.Join(dir, "corrupt.db")
			if err := os.WriteFile(p, []byte("this is not a sqlite database"), 0o600); err != nil {
				t.Fatalf("write corrupt cache fixture: %v", err)
			}
			return p
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newLiteralMCPSource(t, dir)
			src.cfg.Cache = &config.CacheConfig{ExactPath: tc.setup(t)}
			_, err := src.CacheFlush("tenant-a")
			if err == nil {
				t.Fatal("CacheFlush with an unopenable cache file succeeded, want an error")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "mcp: open persistent cache: ") {
				t.Errorf("CacheFlush error = %q, want the %q prefix", msg, "mcp: open persistent cache: ")
			}
			if strings.Contains(msg, dir) {
				t.Errorf("CacheFlush error %q embeds the full cache path %q", msg, dir)
			}
		})
	}
}

func TestRegistryMCPSourceCacheFlushMissingFile(t *testing.T) {
	dir := t.TempDir()
	src := newLiteralMCPSource(t, dir)
	missing := filepath.Join(dir, "missing-cache.db")
	src.cfg.Cache = &config.CacheConfig{ExactPath: missing}
	_, err := src.CacheFlush("tenant-a")
	if err == nil {
		t.Fatal("CacheFlush with a missing cache file succeeded, want an error")
	}
	msg := err.Error()
	// The errno text is platform-specific (Windows reports
	// "The system cannot find the file specified." for ENOENT).
	wantErrno := "no such file or directory"
	if runtime.GOOS == "windows" {
		wantErrno = "The system cannot find the file specified."
	}
	want := `persistent cache file "missing-cache.db" not present: ` + wantErrno
	if msg != "mcp: "+want {
		t.Errorf("CacheFlush error = %q, want %q", msg, "mcp: "+want)
	}
	if strings.Contains(msg, dir) {
		t.Errorf("CacheFlush error %q embeds the full cache path %q — only the basename may appear", msg, dir)
	}
}

// KeyScoped stdio source (M9 follow-up): registryMCPSource must
// implement a2a.KeyScopedComboMetricsSource, so a gateway-key
// caller served by the stdio source gets measured per-key combo
// metrics instead of strategy-only smart-routing. If this line
// ever fails to compile, gateway-key smart-routing silently
// falls back to strategy-only metrics.
var _ a2a.KeyScopedComboMetricsSource = (*registryMCPSource)(nil)

// newComboKeySource builds a registryMCPSource with combo "solo"
// (the one mock provider from newTestMCPSource), prices model-a
// at $10/$30 per 1M in the config price book (model-level entry
// — the live book the source's PriceKnown reads), and returns a
// seed func recording one ledger.Usage row at that price.
// Model "model-b" is deliberately unpriced everywhere (no ledger
// price, no price-book entry), for the unpriced test.
func newComboKeySource(t *testing.T) (*registryMCPSource, string, func(ledger.Usage)) {
	t.Helper()
	src, cfg, _, providerID := newTestMCPSource(t)
	if cfg.Prices == nil {
		cfg.Prices = map[string]config.ModelPrice{}
	}
	cfg.Prices["model-a"] = config.ModelPrice{InputUSDPer1M: fptr(10), OutputUSDPer1M: fptr(30)}
	if err := src.CreateCombo(context.Background(), "solo", []string{providerID}, "priority"); err != nil {
		t.Fatalf("CreateCombo: %v", err)
	}
	led, err := src.openLedger(true)
	if err != nil {
		t.Fatalf("openLedger(true): %v", err)
	}
	t.Cleanup(func() { _ = led.Close() })
	prices := ledger.Prices{
		ledger.PriceKey{Model: "model-a"}: {PromptPerToken: 10.0 / 1e6, CompletionPerToken: 30.0 / 1e6},
	}
	seed := func(u ledger.Usage) {
		t.Helper()
		if _, err := led.RecordUsage(context.Background(), u, prices); err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	return src, providerID, seed
}

// seedComboKeyHistory seeds two keys with deliberately different
// histories for combo "solo" (mirror of the server test's
// newComboKeyServer, adapted to the one-provider combo):
//
//   - key-1: 2 successes on model-a, 1000 prompt + 500 completion
//     each, latencies 120 ms and 80 ms ($0.025 each), plus 1
//     CANCELLED attempt, 100 prompt, 60 ms ($0.001) — a request,
//     never a failure, never a latency event.
//   - key-2: 10 successes on model-a, 100 prompt + 50 completion
//     each, 200 ms each ($0.0025 each), plus 2 FAILURES, 100
//     prompt each ($0.001 each), plus 1 success on a NON-member
//     provider ("other-provider"/"z", 500+500, 10 ms) — must never
//     appear in the combo metrics for any key.
//
// Expected per-key numbers (see the assertions): key-1 requests
// 3 / ok 2 / cost $0.051 / mean latency 100 ms; key-2 requests
// 12 / ok 10 / errors 2 / cost $0.027 / mean latency 200 ms
// over its 10 successful events. The ledger-wide aggregate is
// requests 15 / ok 12 / errors 2 / cost $0.078.
func seedComboKeyHistory(t *testing.T, providerID string, seed func(ledger.Usage)) {
	t.Helper()
	usage := func(keyID, provider, model string, prompt, completion int, ok bool, latency int64) ledger.Usage {
		return ledger.Usage{
			GatewayKeyID: keyID, Provider: provider, Model: model,
			PromptTokens: prompt, CompletionTokens: completion,
			SplitKnown: true, OK: ok, LatencyMS: latency,
		}
	}
	seed(usage("key-1", providerID, "model-a", 1000, 500, true, 120))
	seed(usage("key-1", providerID, "model-a", 1000, 500, true, 80))
	seed(usage("key-1", providerID, "model-a", 100, 0, false, 60).AsCancelled())
	for i := 0; i < 10; i++ {
		seed(usage("key-2", providerID, "model-a", 100, 50, true, 200))
	}
	seed(usage("key-2", providerID, "model-a", 100, 0, false, 50))
	seed(usage("key-2", providerID, "model-a", 100, 0, false, 50))
	seed(usage("key-2", "other-provider", "z", 500, 500, true, 10))
}

// checkRegistryComboMetrics asserts one ComboMetrics result
// field by field (mirror of the server test's
// checkComboMetrics): exact ints, cost within 1e-9, success rate
// nil exactly when there are no requests.
func checkRegistryComboMetrics(t *testing.T, what string, m mcp.ComboMetrics, wantReq, wantOK, wantErr, wantPrompt, wantCompletion, wantTotal int, wantCost float64, wantSR *float64, wantLatKnown bool, wantLat float64, wantLatEvents int) {
	t.Helper()
	if m.Requests != wantReq {
		t.Errorf("%s requests = %d, want %d", what, m.Requests, wantReq)
	}
	if m.OK != wantOK {
		t.Errorf("%s ok = %d, want %d", what, m.OK, wantOK)
	}
	if m.Errors != wantErr {
		t.Errorf("%s errors = %d, want %d", what, m.Errors, wantErr)
	}
	if m.PromptTokens != wantPrompt {
		t.Errorf("%s prompt_tokens = %d, want %d", what, m.PromptTokens, wantPrompt)
	}
	if m.CompletionTokens != wantCompletion {
		t.Errorf("%s completion_tokens = %d, want %d", what, m.CompletionTokens, wantCompletion)
	}
	if m.TotalTokens != wantTotal {
		t.Errorf("%s total_tokens = %d, want %d", what, m.TotalTokens, wantTotal)
	}
	if math.Abs(m.CostUSD-wantCost) > 1e-9 {
		t.Errorf("%s cost_usd = %v, want %v", what, m.CostUSD, wantCost)
	}
	if wantSR == nil {
		if m.SuccessRate != nil {
			t.Errorf("%s success_rate = %v, want nil (no requests)", what, *m.SuccessRate)
		}
	} else if m.SuccessRate == nil {
		t.Errorf("%s success_rate = nil, want %v", what, *wantSR)
	} else if math.Abs(*m.SuccessRate-*wantSR) > 1e-9 {
		t.Errorf("%s success_rate = %v, want %v", what, *m.SuccessRate, *wantSR)
	}
	if m.LatencyKnown != wantLatKnown {
		t.Errorf("%s latency_known = %v, want %v", what, m.LatencyKnown, wantLatKnown)
	}
	if wantLatKnown && math.Abs(m.MeanLatencyMS-wantLat) > 1e-9 {
		t.Errorf("%s mean_latency_ms = %v, want %v", what, m.MeanLatencyMS, wantLat)
	}
	if m.LatencyEvents != wantLatEvents {
		t.Errorf("%s latency_events = %d, want %d", what, m.LatencyEvents, wantLatEvents)
	}
}

// TestRegistryMCPSourceComboMetricsForKeyIsolation: each key sees
// ONLY its own numbers for the same combo — key-1's 3 requests
// must not be polluted by key-2's 12, and key-2's numbers must
// exclude both key-1's rows and its own non-member
// (other-provider/z) row. The cancelled row counts as a request
// for key-1 (with its $0.001 stored cost) but never as an error
// and never as a latency event.
func TestRegistryMCPSourceComboMetricsForKeyIsolation(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seedComboKeyHistory(t, providerID, seed)

	m1, err := src.ComboMetricsForKey("key-1", "solo")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(key-1): %v", err)
	}
	if m1.Name != "solo" || m1.Window != "recent_500_events" {
		t.Errorf("ComboMetricsForKey(key-1): name/window = %q/%q, want solo/recent_500_events", m1.Name, m1.Window)
	}
	if !m1.CostKnown {
		t.Errorf("ComboMetricsForKey(key-1): cost_known = false, want true (model-a is priced)")
	}
	checkRegistryComboMetrics(t, "key-1", m1, 3, 2, 0, 2100, 1000, 3100, 0.051, fptr(2.0/3.0), true, 100, 2)

	m2, err := src.ComboMetricsForKey("key-2", "solo")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(key-2): %v", err)
	}
	if !m2.CostKnown {
		t.Errorf("ComboMetricsForKey(key-2): cost_known = false, want true (the unpriced other-provider/z row is a non-member and must not flip it)")
	}
	if len(m2.UnpricedModels) != 0 {
		t.Errorf("ComboMetricsForKey(key-2): unpriced_models = %v, want none (non-member other-provider/z excluded)", m2.UnpricedModels)
	}
	checkRegistryComboMetrics(t, "key-2", m2, 12, 10, 2, 1200, 500, 1700, 0.027, fptr(10.0/12.0), true, 200, 10)
}

// TestRegistryMCPSourceComboMetricsForKeyEmptyKey: a key with no
// rows for the combo gets zero metrics and no error — never
// another key's numbers, never a zero-filled fake history.
func TestRegistryMCPSourceComboMetricsForKeyEmptyKey(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seedComboKeyHistory(t, providerID, seed)
	m, err := src.ComboMetricsForKey("key-with-nothing", "solo")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(empty key): %v", err)
	}
	checkRegistryComboMetrics(t, "empty key", m, 0, 0, 0, 0, 0, 0, 0, nil, false, 0, 0)
	if !m.CostKnown {
		t.Errorf("ComboMetricsForKey(empty key): cost_known = false, want true (no unpriced tokens seen)")
	}
	if m.Window != "recent_500_events" {
		t.Errorf("ComboMetricsForKey(empty key): window = %q, want recent_500_events", m.Window)
	}
}

// TestRegistryMCPSourceComboMetricsForKeyWhitespaceKeyID: a
// padded key id (" key-1 ") is normalised before the ledger
// comparison, so it returns that key's metrics — never a silent
// zero for a key that does exist.
func TestRegistryMCPSourceComboMetricsForKeyWhitespaceKeyID(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seedComboKeyHistory(t, providerID, seed)
	m, err := src.ComboMetricsForKey(" key-1 ", "solo")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(\" key-1 \"): %v", err)
	}
	checkRegistryComboMetrics(t, "padded key-1", m, 3, 2, 0, 2100, 1000, 3100, 0.051, fptr(2.0/3.0), true, 100, 2)
}

// TestRegistryMCPSourceComboMetricsForKeyErrors: unknown combo
// and empty key id are invalid input; no config / no ledger is
// an honest unavailable error, never a zero result. Validation
// order is pinned too: an empty key id beats a missing config.
func TestRegistryMCPSourceComboMetricsForKeyErrors(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seedComboKeyHistory(t, providerID, seed)
	if _, err := src.ComboMetricsForKey("key-1", "no-such-combo"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(unknown combo): err = %v, want invalid input", err)
	}
	if _, err := src.ComboMetricsForKey("", "solo"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(empty key): err = %v, want invalid input", err)
	}
	if _, err := src.ComboMetricsForKey("  ", "solo"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(blank key): err = %v, want invalid input", err)
	}
	// No config at all: honest unavailability — and the empty
	// key check still wins over it.
	bare := &registryMCPSource{}
	if _, err := bare.ComboMetricsForKey("key-1", "solo"); err == nil || !strings.Contains(err.Error(), "no config loaded") {
		t.Errorf("ComboMetricsForKey(no config): err = %v, want a no-config-loaded error", err)
	}
	if _, err := bare.ComboMetricsForKey("", "solo"); !errors.Is(err, mcp.ErrInvalidInput) {
		t.Errorf("ComboMetricsForKey(no config, empty key): err = %v, want invalid input (empty key wins)", err)
	}
	// No ledger yet (combo exists, ledger never opened): same
	// honest unavailability as ComboMetrics, never a silent
	// empty result.
	noLedger, _, _, noLedgerProviderID := newTestMCPSource(t)
	if err := noLedger.CreateCombo(context.Background(), "solo", []string{noLedgerProviderID}, "priority"); err != nil {
		t.Fatalf("CreateCombo: %v", err)
	}
	if _, err := noLedger.ComboMetricsForKey("key-1", "solo"); err == nil || !strings.Contains(err.Error(), "ledger unavailable") {
		t.Errorf("ComboMetricsForKey(no ledger): err = %v, want a ledger-unavailable error", err)
	}
}

// TestRegistryMCPSourceComboMetricsForKeyUnpriced: a row on an
// unpriced model (model-b — no ledger price, no price-book
// entry) marks the cost unknown with exactly that model's tokens
// listed; the priced rows still contribute their measured cost.
func TestRegistryMCPSourceComboMetricsForKeyUnpriced(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seed(ledger.Usage{
		GatewayKeyID: "key-1", Provider: providerID, Model: "model-a",
		PromptTokens: 1000, CompletionTokens: 500,
		SplitKnown: true, OK: true, LatencyMS: 50,
	}) // priced: $0.025
	seed(ledger.Usage{
		GatewayKeyID: "key-1", Provider: providerID, Model: "model-b",
		PromptTokens: 500, CompletionTokens: 100,
		SplitKnown: true, OK: true, LatencyMS: 50,
	}) // unpriced: $0 stored, 600 tokens
	m, err := src.ComboMetricsForKey("key-1", "solo")
	if err != nil {
		t.Fatalf("ComboMetricsForKey(unpriced): %v", err)
	}
	checkRegistryComboMetrics(t, "unpriced", m, 2, 2, 0, 1500, 600, 2100, 0.025, fptr(1.0), true, 50, 2)
	if m.CostKnown {
		t.Errorf("unpriced: cost_known = true, want false (model-b is unpriced)")
	}
	if m.UnpricedTokens != 600 {
		t.Errorf("unpriced: unpriced_tokens = %d, want 600 (model-b's tokens only)", m.UnpricedTokens)
	}
	wantModel := providerID + "/model-b"
	if len(m.UnpricedModels) != 1 || m.UnpricedModels[0] != wantModel {
		t.Errorf("unpriced: unpriced_models = %v, want [%s]", m.UnpricedModels, wantModel)
	}
}

// TestRegistryMCPSourceComboMetricsLedgerWideUnchanged: the
// existing ledger-wide ComboMetrics still returns the aggregate
// over BOTH keys (plus neither key's non-member row) — the new
// per-key method changed no ledger-wide behaviour.
func TestRegistryMCPSourceComboMetricsLedgerWideUnchanged(t *testing.T) {
	src, providerID, seed := newComboKeySource(t)
	seedComboKeyHistory(t, providerID, seed)
	m, err := src.ComboMetrics("solo")
	if err != nil {
		t.Fatalf("ComboMetrics(solo): %v", err)
	}
	if m.Window != "all_time" {
		t.Errorf("ComboMetrics: window = %q, want all_time (ledger-wide is unchanged)", m.Window)
	}
	if !m.CostKnown {
		t.Errorf("ComboMetrics: cost_known = false, want true")
	}
	// 15 requests = key-1's 3 + key-2's 12; the
	// other-provider/z row is a non-member and stays out.
	// Latency mean is (120+80+10*200)/12 = 2200/12.
	checkRegistryComboMetrics(t, "ledger-wide", m, 15, 12, 2, 3300, 1500, 4800, 0.078, fptr(12.0/15.0), true, 2200.0/12.0, 12)
}
