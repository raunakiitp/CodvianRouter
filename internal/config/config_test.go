package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig("/tmp/data")
	if cfg.Version != CurrentVersion {
		t.Fatalf("Version = %d, want %d", cfg.Version, CurrentVersion)
	}
	if cfg.Listen != DefaultListen {
		t.Fatalf("Listen = %q, want %q", cfg.Listen, DefaultListen)
	}
	if cfg.DataDir != "/tmp/data" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if !cfg.FailClosed {
		t.Error("FailClosed should default to true")
	}
	if cfg.TelemetryEnabled {
		t.Error("TelemetryEnabled should default to false")
	}
	if cfg.BudgetUSD != 0 {
		t.Errorf("BudgetUSD = %v, want 0 (no cap)", cfg.BudgetUSD)
	}
	if len(cfg.GatewayKeys) != 0 || len(cfg.Providers) != 0 {
		t.Error("fresh config should have no keys or providers")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig failed Validate: %v", err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := DefaultConfig(filepath.Join(dir, "data"))
	cfg.BudgetUSD = 25.5
	cfg.Providers = []ProviderConfig{
		{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1", APIKeyEncrypted: "v1:abc", WireFormat: "openai", Models: []provider.Model{{ID: "gpt-4o", Name: "GPT-4o"}}, Enabled: true},
	}
	key, raw, err := IssueKey("test", []string{"chat"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}

	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Saved file must be 0600 and valid JSON with the expected top-level keys.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("config file perms = %o, want 0600", info.Mode().Perm())
	}
	rawJSON, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(rawJSON, &decoded); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	for _, want := range []string{"version", "listen", "data_dir", "gateway_keys", "providers", "budget_usd", "fail_closed", "telemetry_enabled"} {
		if _, ok := decoded[want]; !ok {
			t.Errorf("saved JSON missing field %q", want)
		}
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.BudgetUSD != 25.5 || loaded.Listen != DefaultListen {
		t.Errorf("loaded config mismatch: %+v", loaded)
	}
	if len(loaded.Providers) != 1 || loaded.Providers[0].ID != "openai" {
		t.Errorf("loaded providers mismatch: %+v", loaded.Providers)
	}
	if loaded.Providers[0].WireFormat != "openai" || len(loaded.Providers[0].Models) != 1 || loaded.Providers[0].Models[0].ID != "gpt-4o" {
		t.Errorf("loaded provider fields mismatch (wire_format/models): %+v", loaded.Providers[0])
	}
	if len(loaded.GatewayKeys) != 1 {
		t.Fatalf("loaded gateway keys = %d, want 1", len(loaded.GatewayKeys))
	}
	if got, ok := loaded.VerifyGatewayKey(raw); !ok || got.ID != key.ID {
		t.Error("issued raw key does not verify after save/load round-trip")
	}
	// No stray temp files left behind by the atomic write.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".config-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err == nil {
		t.Fatal("Load of a missing file should error")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Load error should wrap os.ErrNotExist, got: %v", err)
	}
}

func TestLoadRejectsBadVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := DefaultConfig(t.TempDir())
	cfg.Version = 99
	// Bypass Save's Validate by writing the JSON directly.
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load should reject version 99")
	}
}

func TestLoadRejectsBadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load should reject invalid JSON")
	}
}

func TestValidateRejectsNonLoopback(t *testing.T) {
	for _, listen := range []string{"0.0.0.0:20128", "192.168.1.5:20128", "example.com:20128", "localhost:bad", ":20128"} {
		cfg := DefaultConfig(t.TempDir())
		cfg.Listen = listen
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate should reject listen %q", listen)
		}
	}
}

func TestValidateAcceptsLoopbackForms(t *testing.T) {
	for _, listen := range []string{"127.0.0.1:20128", "127.0.0.1:8080", "[::1]:20128"} {
		cfg := DefaultConfig(t.TempDir())
		cfg.Listen = listen
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate should accept listen %q: %v", listen, err)
		}
	}
}

func TestValidateRejectsNegativeBudget(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.BudgetUSD = -1
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject a negative budget")
	}
}

func TestLoadOrCreate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json")
	cfg, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate (missing): %v", err)
	}
	if cfg.Listen != DefaultListen {
		t.Errorf("created config Listen = %q", cfg.Listen)
	}
	again, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("LoadOrCreate (existing): %v", err)
	}
	if again.Listen != cfg.Listen {
		t.Error("LoadOrCreate should return the existing config on the second call")
	}
}

func TestSetListenPort(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	if err := cfg.SetListenPort(9999); err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9999" {
		t.Errorf("Listen = %q, want 127.0.0.1:9999", cfg.Listen)
	}
	if p, err := cfg.ListenPort(); err != nil || p != 9999 {
		t.Errorf("ListenPort = %d, err = %v", p, err)
	}
	for _, bad := range []int{0, -1, 65536} {
		if err := cfg.SetListenPort(bad); err == nil {
			t.Errorf("SetListenPort(%d) should error", bad)
		}
	}
}

func TestSaveRefusesInvalidConfig(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Listen = "0.0.0.0:20128"
	if err := cfg.Save(filepath.Join(t.TempDir(), "config.json")); err == nil {
		t.Error("Save should refuse a non-loopback listen address")
	}
}
