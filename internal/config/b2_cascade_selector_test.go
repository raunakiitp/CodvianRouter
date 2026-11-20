package config

// B2-542 selector tests (S1-B2-542-SELECTOR, 2026-11-20).
//
// global_strategy="cascade" must load from a real config file —
// before the allowlist entry, Load failed with "not a known
// strategy". The routing proof (the loaded value drives the
// cascade order at dispatch) is
// TestB2CascadeSelectorRoutesFromConfigFile in internal/router.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestB2CascadeGlobalStrategyLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"version":1,"listen":"127.0.0.1:20128","data_dir":"/tmp/data","routing":{"global_strategy":"cascade"}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load global_strategy=cascade: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.GlobalStrategy != "cascade" {
		t.Fatalf("loaded global_strategy = %v, want cascade", loaded.Routing)
	}
}

func TestB2CascadeChainStrategyLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := `{"version":1,"listen":"127.0.0.1:20128","data_dir":"/tmp/data","routing":{"chain_strategies":{"m":"cascade"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load chain_strategies[m]=cascade: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.ChainStrategies["m"] != "cascade" {
		t.Fatalf("loaded chain_strategies = %v, want {m: cascade}", loaded.Routing)
	}
}
