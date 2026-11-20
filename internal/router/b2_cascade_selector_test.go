package router

// B2-542 selector test (S1-B2-542-SELECTOR, 2026-11-20).
//
// The selector gap, end to end: a real config file naming
// global_strategy="cascade" loads, the loaded value becomes the
// engine's configured strategy (the main.go:3582 assignment), and
// a real dispatch with no per-request strategy serves the
// subscription tier first — the engine resolves cfg.Strategy
// through effectiveStrategy and intercepts "cascade" before the
// hook. Before the allowlist entry, config.Load failed with "not
// a known strategy", so this test FAILS when the entry is removed.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestB2CascadeSelectorRoutesFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"version":1,"listen":"127.0.0.1:20128","data_dir":"/tmp/data","routing":{"global_strategy":"cascade"}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load global_strategy=cascade: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.GlobalStrategy != "cascade" {
		t.Fatalf("loaded global_strategy = %v, want cascade", loaded.Routing)
	}

	cfg := testConfig()
	cfg.Strategy = loaded.Routing.GlobalStrategy
	installAutoCatalog(t, b2CascadeCatalog())

	// The loaded value resolves as the effective strategy.
	eng := NewEngine(nil, nil, cfg)
	if got := eng.effectiveStrategy("", "m"); got != "cascade" {
		t.Fatalf("effectiveStrategy() = %q, want cascade (loaded global_strategy)", got)
	}
	want := []string{"provS", "provA", "provC", "provF", "provK"}
	ordered, zeroSkipped := eng.orderAttemptsWithContext(b2CascadeChain(), eng.effectiveStrategy("", "m"), "hard", false, StrategyHookContext{})
	if len(zeroSkipped) != 0 {
		t.Fatalf("cascade from config: zeroSkipped = %v, want none", providerIDs(zeroSkipped))
	}
	if got := providerIDs(ordered); len(got) != len(want) {
		t.Fatalf("cascade from config order = %v, want %v", got, want)
	} else {
		for i, w := range want {
			if got[i] != w {
				t.Fatalf("cascade from config order = %v, want %v", got, want)
			}
		}
	}

	// End to end: a real dispatch with no per-request strategy
	// serves the subscription tier first via cfg.Strategy.
	clients := map[string]provider.Client{
		"provS": okClient("from-sub"),
		"provA": okClient("from-api"),
		"provC": okClient("from-cheap"),
		"provF": okClient("from-free"),
		"provK": okClient("from-keyless"),
	}
	eng2 := NewEngine(clients, nil, cfg)
	resp, _, err := eng2.ChatWithChainStrategy(context.Background(), b2CascadeChain(), qsReq(), "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy (config cascade) error: %v", err)
	}
	if resp.Content != "from-sub" {
		t.Fatalf("config-cascade dispatch served %q, want from-sub (subscription tier first)", resp.Content)
	}

	// Control: the same chain under "balanced" keeps input order.
	control, _ := eng.orderAttemptsWithContext(b2CascadeChain(), "balanced", "hard", false, StrategyHookContext{})
	if got := providerIDs(control); len(got) != 5 || got[0] != "provC" || got[4] != "provS" {
		t.Fatalf("balanced control order = %v, want the input order [provC provF provK provA provS]", got)
	}
}
