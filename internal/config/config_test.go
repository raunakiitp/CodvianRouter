package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/strategytree"
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
	// RT-1: a fresh config carries a 300 s per-combo deadline
	// budget by default — no request may run unbounded.
	if cfg.Routing == nil {
		t.Fatal("Routing should be non-nil in a fresh config (carries the default deadline budget)")
	}
	if cfg.Routing.DeadlineBudgetMs != DefaultDeadlineBudgetMs {
		t.Errorf("Routing.DeadlineBudgetMs = %d, want %d (5 minutes)", cfg.Routing.DeadlineBudgetMs, DefaultDeadlineBudgetMs)
	}
	if DefaultDeadlineBudgetMs != 300000 {
		t.Errorf("DefaultDeadlineBudgetMs = %d, want 300000", DefaultDeadlineBudgetMs)
	}
	if len(cfg.GatewayKeys) != 0 || len(cfg.Providers) != 0 {
		t.Error("fresh config should have no keys or providers")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("DefaultConfig failed Validate: %v", err)
	}
}

// TestDefaultDeadlineBudgetExplicitWins (RT-1): an explicit
// routing.deadline_budget_ms in a config file wins over the 300 s
// default, and an explicit 0 still disables the budget — the
// default applies only when the operator sets nothing. The
// default also survives a Save/Load round-trip.
func TestDefaultDeadlineBudgetExplicitWins(t *testing.T) {
	writeJSON := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// Explicit 60000 ms survives Load untouched.
	explicit := writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"deadline_budget_ms": 60000}}`)
	loaded, err := Load(explicit)
	if err != nil {
		t.Fatalf("Load explicit budget: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != 60000 {
		t.Fatalf("explicit deadline_budget_ms = %v, want 60000", loaded.Routing)
	}
	// Explicit 0 still means disabled (main.go applies the
	// budget only when > 0) — the default must not override it.
	disabled := writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"deadline_budget_ms": 0}}`)
	loaded, err = Load(disabled)
	if err != nil {
		t.Fatalf("Load explicit 0 budget: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != 0 {
		t.Fatalf("explicit deadline_budget_ms 0 = %v, want 0 (disabled)", loaded.Routing)
	}
	// The DefaultConfig default survives Save + Load.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := DefaultConfig(filepath.Join(dir, "data")).Save(path); err != nil {
		t.Fatalf("Save DefaultConfig: %v", err)
	}
	loaded, err = Load(path)
	if err != nil {
		t.Fatalf("Load saved DefaultConfig: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != DefaultDeadlineBudgetMs {
		t.Fatalf("saved default deadline_budget_ms = %v, want %d", loaded.Routing, DefaultDeadlineBudgetMs)
	}
}

// TestLoadFillsDefaultDeadlineBudgetWhenOmitted (RT-1, Block 1
// B1): a config file that omits routing.deadline_budget_ms — or
// omits routing entirely — loads with the 300 s default instead
// of 0 (no budget at all). An explicit value is never
// overridden, including an explicit 0, which disables the budget.
func TestLoadFillsDefaultDeadlineBudgetWhenOmitted(t *testing.T) {
	writeJSON := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// routing present but the field omitted.
	loaded, err := Load(writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {}}`))
	if err != nil {
		t.Fatalf("Load routing {}: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != DefaultDeadlineBudgetMs {
		t.Fatalf("omitted deadline_budget_ms = %v, want %d", loaded.Routing, DefaultDeadlineBudgetMs)
	}
	// routing key absent entirely.
	loaded, err = Load(writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data"}`))
	if err != nil {
		t.Fatalf("Load without routing: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != DefaultDeadlineBudgetMs {
		t.Fatalf("absent routing deadline_budget_ms = %v, want %d", loaded.Routing, DefaultDeadlineBudgetMs)
	}
	// Explicit non-zero value preserved.
	loaded, err = Load(writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"deadline_budget_ms": 60000}}`))
	if err != nil {
		t.Fatalf("Load explicit 60000: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != 60000 {
		t.Fatalf("explicit deadline_budget_ms = %v, want 60000", loaded.Routing)
	}
	// Explicit 0 still disables the budget.
	loaded, err = Load(writeJSON(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"deadline_budget_ms": 0}}`))
	if err != nil {
		t.Fatalf("Load explicit 0: %v", err)
	}
	if loaded.Routing == nil || loaded.Routing.DeadlineBudgetMs != 0 {
		t.Fatalf("explicit deadline_budget_ms 0 = %v, want 0 (disabled)", loaded.Routing)
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
	// Windows has no Unix permission bits (files report 0666), so
	// the exact mode is only asserted off Windows.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
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

func TestValidateCatalogProviderMayOmitBaseURL(t *testing.T) {
	// A catalog-linked provider resolves its base URL from the
	// catalog at load, so an empty base_url is valid for it — but a
	// fully-custom provider with no base URL is still rejected.
	cfg := DefaultConfig(t.TempDir())
	cfg.Providers = []ProviderConfig{{ID: "prov-groq", Name: "Groq", CatalogID: "groq"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate should accept a catalog provider with no base_url: %v", err)
	}

	cfg.Providers = []ProviderConfig{{ID: "prov-custom", Name: "Custom"}}
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject a custom provider with no base_url")
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

func TestUnknownFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	raw := []byte(`{
  "version": 1,
  "listen": "127.0.0.1:20128",
  "data_dir": "/tmp/data",
  "gateway_keys": [],
  "providers": [],
  "budget_usd": 0,
  "fail_closed": true,
  "telemetry_enabled": false,
  "future_feature": {"enabled": true, "level": 3},
  "ui_theme": "dark",
  "max_retries": 5,
  "ratio": 1.5,
  "notes": null,
  "tags": ["a", "b"],
  "experimental": true
}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]any{
		"future_feature": map[string]any{"enabled": true, "level": float64(3)},
		"ui_theme":       "dark",
		"max_retries":    float64(5),
		"ratio":          1.5,
		"notes":          nil,
		"tags":           []any{"a", "b"},
		"experimental":   true,
	}
	if len(loaded.Extensions) != len(want) {
		t.Fatalf("Extensions = %d entries, want %d: %v", len(loaded.Extensions), len(want), loaded.Extensions)
	}
	for k, w := range want {
		var got any
		if err := json.Unmarshal(loaded.Extensions[k], &got); err != nil {
			t.Fatalf("Extensions[%q] is not valid JSON: %v", k, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("Extensions[%q] = %#v, want %#v", k, got, w)
		}
	}
	// MarshalJSON must re-emit an unknown value byte-for-byte (verbatim
	// raw bytes, including its original spacing). Note the top-level
	// json.Marshal compacts Marshaler output, so the verbatim check has
	// to call MarshalJSON directly; via json.Marshal the same value
	// appears in compacted form, still value-identical.
	rawOut, err := loaded.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawOut), `"future_feature":{"enabled": true, "level": 3}`) {
		t.Errorf("MarshalJSON did not preserve future_feature verbatim: %s", rawOut)
	}
	encoded, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"future_feature":{"enabled":true,"level":3}`) {
		t.Errorf("Marshal did not preserve future_feature value: %s", encoded)
	}
	// Save -> Load must preserve the unknown values, and "extensions"
	// itself must never appear as a top-level key in the file.
	savedPath := filepath.Join(dir, "saved.json")
	if err := loaded.Save(savedPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	savedRaw, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(savedRaw, &saved); err != nil {
		t.Fatalf("saved config is not valid JSON: %v", err)
	}
	if _, ok := saved["extensions"]; ok {
		t.Error("saved JSON must not contain an \"extensions\" key")
	}
	for k, w := range want {
		if !reflect.DeepEqual(saved[k], w) {
			t.Errorf("saved[%q] = %#v, want %#v", k, saved[k], w)
		}
	}
	if saved["version"] != float64(1) || saved["listen"] != DefaultListen {
		t.Errorf("known fields changed across round-trip: %v", saved)
	}
	reloaded, err := Load(savedPath)
	if err != nil {
		t.Fatalf("Load(saved): %v", err)
	}
	for k, w := range want {
		var got any
		if err := json.Unmarshal(reloaded.Extensions[k], &got); err != nil {
			t.Fatalf("reloaded Extensions[%q] is not valid JSON: %v", k, err)
		}
		if !reflect.DeepEqual(got, w) {
			t.Errorf("reloaded Extensions[%q] = %#v, want %#v", k, got, w)
		}
	}
}

func TestUnknownFieldsAbsentWhenNone(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	if cfg.Extensions != nil {
		t.Error("fresh config should have nil Extensions")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	// RT-1: the fresh config now carries a routing block (the
	// default 300 s deadline budget), so 10 known top-level
	// keys — the exact set is pinned so a stray/unknown field
	// still fails this test.
	wantKeys := map[string]bool{
		"auto_router": true, "budget_usd": true, "data_dir": true,
		"fail_closed": true, "gateway_keys": true, "listen": true,
		"providers": true, "routing": true, "telemetry_enabled": true,
		"version": true,
	}
	if len(decoded) != len(wantKeys) {
		t.Errorf("encoded config has %d top-level keys, want %d: %v", len(decoded), len(wantKeys), decoded)
	}
	for k := range decoded {
		if !wantKeys[k] {
			t.Errorf("encoded config has unexpected top-level key %q", k)
		}
	}
	for k := range wantKeys {
		if _, ok := decoded[k]; !ok {
			t.Errorf("encoded config missing top-level key %q", k)
		}
	}
}

func writeTempConfig(t *testing.T, cfg *Config) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateFile(t *testing.T) {
	twoKeys := func() (*Config, string) {
		cfg := DefaultConfig(t.TempDir())
		k1, _, err := IssueKey("one", []string{"chat"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		k2, _, err := IssueKey("two", []string{"chat"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		k2.ID = k1.ID
		_ = cfg.AddGatewayKey(k1)
		// AddGatewayKey would reject the duplicate, so append directly.
		cfg.GatewayKeys = append(cfg.GatewayKeys, k2)
		return cfg, k1.ID
	}
	dupKeysCfg, dupKeyID := twoKeys()

	cases := []struct {
		name string
		cfg  *Config
		want string // substring the error must contain; "" means no error
	}{
		{"valid", DefaultConfig(t.TempDir()), ""},
		{"version", func() *Config { c := DefaultConfig(t.TempDir()); c.Version = 2; return c }(), "version"},
		{"listen not loopback", func() *Config { c := DefaultConfig(t.TempDir()); c.Listen = "0.0.0.0:20128"; return c }(), "listen"},
		{"negative budget", func() *Config { c := DefaultConfig(t.TempDir()); c.BudgetUSD = -5; return c }(), "budget_usd"},
		{"duplicate gateway key id", dupKeysCfg, `duplicate gateway key id "` + dupKeyID + `"`},
		{"duplicate provider id", func() *Config {
			c := DefaultConfig(t.TempDir())
			c.Providers = []ProviderConfig{
				{ID: "openai", BaseURL: "https://api.openai.com/v1"},
				{ID: "openai", BaseURL: "https://api.other.com/v1"},
			}
			return c
		}(), `duplicate provider id "openai"`},
		{"duplicate provider base_url normalized", func() *Config {
			c := DefaultConfig(t.TempDir())
			c.Providers = []ProviderConfig{
				{ID: "a", BaseURL: "https://api.example.com/v1/"},
				{ID: "b", BaseURL: "HTTPS://API.EXAMPLE.COM/v1"},
			}
			return c
		}(), "duplicate provider base_url"},
		{"provider empty id", func() *Config {
			c := DefaultConfig(t.TempDir())
			c.Providers = []ProviderConfig{{ID: "", BaseURL: "https://api.example.com/v1"}}
			return c
		}(), "empty id"},
		{"provider empty base_url", func() *Config {
			c := DefaultConfig(t.TempDir())
			c.Providers = []ProviderConfig{{ID: "p1", BaseURL: ""}}
			return c
		}(), "empty base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateFile(writeTempConfig(t, tc.cfg))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateFile(valid) = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("ValidateFile(%s) = nil, want error containing %q", tc.name, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ValidateFile(%s) error = %v, want it to contain %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestValidateFileBadInput(t *testing.T) {
	if err := ValidateFile(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Error("ValidateFile of a missing file should error")
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFile(path); err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Errorf("ValidateFile of invalid JSON should report invalid JSON, got: %v", err)
	}
}

func TestCloneIndependent(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.BudgetUSD = 10
	key, _, err := IssueKey("test", []string{"chat"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatal(err)
	}
	cfg.Providers = []ProviderConfig{
		{ID: "openai", BaseURL: "https://api.openai.com/v1", Models: []provider.Model{{ID: "gpt-4o", Name: "GPT-4o"}}},
	}
	cfg.Extensions = map[string]json.RawMessage{
		"future": json.RawMessage(`{"a":1}`),
		"list":   json.RawMessage(`[1,2]`),
	}

	clone := cfg.Clone()
	if clone == cfg {
		t.Fatal("Clone must return a new pointer")
	}
	clone.Listen = "127.0.0.1:9999"
	clone.BudgetUSD = 99
	clone.GatewayKeys[0].Name = "changed"
	clone.GatewayKeys[0].Scopes[0] = "changed"
	clone.GatewayKeys = append(clone.GatewayKeys, GatewayKey{ID: "extra"})
	clone.Providers[0].ID = "changed"
	clone.Providers[0].Models[0].ID = "changed"
	clone.Providers = append(clone.Providers, ProviderConfig{ID: "extra"})
	clone.Extensions["future"] = json.RawMessage(`{"a":2}`)
	clone.Extensions["new"] = json.RawMessage(`true`)
	listBytes := clone.Extensions["list"]
	listBytes[1] = '9' // mutates the clone's raw bytes in place

	if cfg.Listen != DefaultListen {
		t.Errorf("original Listen mutated: %q", cfg.Listen)
	}
	if cfg.BudgetUSD != 10 {
		t.Errorf("original BudgetUSD mutated: %v", cfg.BudgetUSD)
	}
	if len(cfg.GatewayKeys) != 1 || cfg.GatewayKeys[0].Name != "test" {
		t.Errorf("original gateway keys mutated: %+v", cfg.GatewayKeys)
	}
	if cfg.GatewayKeys[0].Scopes[0] != "chat" {
		t.Errorf("original gateway key scopes mutated: %v", cfg.GatewayKeys[0].Scopes)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].ID != "openai" {
		t.Errorf("original providers mutated: %+v", cfg.Providers)
	}
	if cfg.Providers[0].Models[0].ID != "gpt-4o" {
		t.Errorf("original provider models mutated: %+v", cfg.Providers[0].Models)
	}
	if string(cfg.Extensions["future"]) != `{"a":1}` {
		t.Errorf("original Extensions[future] mutated: %s", cfg.Extensions["future"])
	}
	if string(cfg.Extensions["list"]) != `[1,2]` {
		t.Errorf("original Extensions[list] mutated: %s", cfg.Extensions["list"])
	}
	if _, ok := cfg.Extensions["new"]; ok {
		t.Error("original Extensions gained the clone's new key")
	}

	var nilCfg *Config
	if nilCfg.Clone() != nil {
		t.Error("Clone of a nil config should be nil")
	}
}

func TestConfigAutoRouterValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		return cfg
	}
	// Defaults are valid.
	if err := base().Validate(); err != nil {
		t.Errorf("DefaultConfig failed Validate: %v", err)
	}
	// A full auto_router block round-trips through JSON.
	cfg := base()
	cfg.AutoRouter = AutoRouterConfig{
		Threshold:     0.6,
		Thresholds:    map[string]float64{"auto/fast": 0.7},
		DefaultTier:   "coding",
		DefaultModel:  "strong-model",
		TierModels:    map[string][]string{"coding": {"coder-model"}, "chat": {"chat-model"}},
		IntentTiers:   map[string]IntentTierConfig{"fast": {Model: "cheap-model", Effort: "low"}, "best": {Model: "strong-model", Effort: "high"}},
		LearnedRouter: true,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.AutoRouter.Threshold != 0.6 || back.AutoRouter.Thresholds["auto/fast"] != 0.7 ||
		back.AutoRouter.DefaultTier != "coding" || back.AutoRouter.DefaultModel != "strong-model" ||
		!back.AutoRouter.LearnedRouter ||
		len(back.AutoRouter.TierModels["coding"]) != 1 ||
		back.AutoRouter.IntentTiers["fast"] != (IntentTierConfig{Model: "cheap-model", Effort: "low"}) ||
		back.AutoRouter.IntentTiers["best"] != (IntentTierConfig{Model: "strong-model", Effort: "high"}) {
		t.Errorf("auto_router did not round-trip: %+v", back.AutoRouter)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	// Clone deep-copies the auto-router maps.
	clone := cfg.Clone()
	clone.AutoRouter.Thresholds["auto/fast"] = 0.1
	clone.AutoRouter.TierModels["coding"][0] = "other-model"
	clone.AutoRouter.IntentTiers["fast"] = IntentTierConfig{Model: "other-cheap", Effort: "low"}
	if cfg.AutoRouter.Thresholds["auto/fast"] != 0.7 {
		t.Error("Clone shared Thresholds map with the original")
	}
	if cfg.AutoRouter.TierModels["coding"][0] != "coder-model" {
		t.Error("Clone shared TierModels slices with the original")
	}
	if cfg.AutoRouter.IntentTiers["fast"].Model != "cheap-model" {
		t.Error("Clone shared IntentTiers map with the original")
	}
	// Invalid values are rejected, naming the field.
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"threshold > 1", func(c *Config) { c.AutoRouter.Threshold = 1.5 }},
		{"threshold < 0", func(c *Config) { c.AutoRouter.Threshold = -0.1 }},
		{"per-model threshold 0", func(c *Config) { c.AutoRouter.Thresholds = map[string]float64{"auto": 0} }},
		{"unknown virtual model", func(c *Config) { c.AutoRouter.Thresholds = map[string]float64{"auto/nope": 0.5} }},
		{"unknown default tier", func(c *Config) { c.AutoRouter.DefaultTier = "magic" }},
		{"unknown tier_models tier", func(c *Config) { c.AutoRouter.TierModels = map[string][]string{"magic": {"m"}} }},
		{"unknown intent tier", func(c *Config) { c.AutoRouter.IntentTiers = map[string]IntentTierConfig{"turbo": {Model: "m"}} }},
		{"intent tier empty model", func(c *Config) { c.AutoRouter.IntentTiers = map[string]IntentTierConfig{"fast": {Model: ""}} }},
		{"intent tier unknown effort", func(c *Config) {
			c.AutoRouter.IntentTiers = map[string]IntentTierConfig{"fast": {Model: "m", Effort: "huge"}}
		}},
	}
	for _, tc := range bad {
		c := base()
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate passed; want an error naming the field", tc.name)
		}
	}
	// Threshold 0 (unset) is valid: it means "use the 0.5 default".
	c := base()
	c.AutoRouter.Threshold = 0
	if err := c.Validate(); err != nil {
		t.Errorf("threshold 0 (unset) failed Validate: %v", err)
	}
}

// TestConfigGuardrailsRoundTrip proves the guardrails blocking
// toggles persist through JSON (config file) and that the default
// config leaves all blocking off (log redaction is not a config
// value — it is always on).
func TestConfigGuardrailsRoundTrip(t *testing.T) {
	if c := DefaultConfig(t.TempDir()); c.Guardrails != nil {
		t.Fatalf("default config must leave guardrails unset (nil): %+v", c.Guardrails)
	}
	cfg := DefaultConfig(t.TempDir())
	cfg.Guardrails = &GuardrailsConfig{BlockPII: true, BlockSecrets: false, BlockInjection: true}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !back.Guardrails.BlockPII || back.Guardrails.BlockSecrets || !back.Guardrails.BlockInjection {
		t.Fatalf("guardrails did not round-trip: %+v", back.Guardrails)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("config with guardrails failed Validate: %v", err)
	}
}

// TestConfigGuardrailsExternalValidation proves the async
// guardrail mode is validated at load (CHECKLIST 185): async +
// a blocking kind (pii/secret/injection) is rejected naming the
// guardrail, while sync blocking kinds and async classifier kinds
// pass — and that external entries round-trip through JSON and
// Clone deep-copies them.
func TestConfigGuardrailsExternalValidation(t *testing.T) {
	base := func() *Config { return DefaultConfig(t.TempDir()) }
	// Async + blocking kind fails, naming the guardrail.
	for _, kind := range []string{"pii", "secret", "injection"} {
		c := base()
		c.Guardrails = &GuardrailsConfig{External: []ExternalGuardrailConfig{
			{Name: "slow-" + kind, Kind: kind, Mode: "async", Endpoint: "http://127.0.0.1:9/check"},
		}}
		if err := c.Validate(); err == nil {
			t.Fatalf("async + blocking kind %q must fail Validate", kind)
		} else if !strings.Contains(err.Error(), "slow-"+kind) {
			t.Fatalf("async + %q error should name the guardrail, got: %v", kind, err)
		}
	}
	// Valid: sync blocking kind + async classifier kind.
	c := base()
	c.Guardrails = &GuardrailsConfig{External: []ExternalGuardrailConfig{
		{Name: "pii-check", Kind: "pii", Mode: "sync", Endpoint: "http://127.0.0.1:9/check"},
		{Name: "toxicity", Kind: "toxicity", Mode: "async", Endpoint: "http://127.0.0.1:9/check", TimeoutMs: 800},
	}}
	if err := c.Validate(); err != nil {
		t.Fatalf("valid external guardrails failed Validate: %v", err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Guardrails.External) != 2 || back.Guardrails.External[1].TimeoutMs != 800 {
		t.Fatalf("external guardrails did not round-trip: %+v", back.Guardrails.External)
	}
	// Clone deep-copies the slice: mutating the clone's entry must
	// not touch the original.
	clone := c.Clone()
	clone.Guardrails.External[0].Name = "renamed"
	if c.Guardrails.External[0].Name != "pii-check" {
		t.Fatalf("Clone shared the external slice: original now %q", c.Guardrails.External[0].Name)
	}
}
func TestConfigRoutingValidation(t *testing.T) {
	base := func() *Config { return DefaultConfig(t.TempDir()) }
	if err := base().Validate(); err != nil {
		t.Errorf("DefaultConfig failed Validate: %v", err)
	}
	cfg := base()
	cfg.Routing = &RoutingConfig{
		GlobalStrategy:        "cheapest",
		DeadlineBudgetMs:      2000,
		ChainStrategies:       map[string]string{"gpt-4o": "fastest", "claude-x": "most-reliable"},
		WeightedWeights:       &RoutingWeights{Latency: 0.7, Cost: 0.3},
		CooldownMode:          "soft-demote",
		ChainCooldownModes:    map[string]string{"gpt-4o": "hard"},
		ContextFallbackModels: []string{"gpt-4o-long", "claude-long"},
		ContextReserveTokens:  2048,
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Routing.GlobalStrategy != "cheapest" || back.Routing.DeadlineBudgetMs != 2000 ||
		back.Routing.ChainStrategies["gpt-4o"] != "fastest" ||
		back.Routing.ChainStrategies["claude-x"] != "most-reliable" ||
		back.Routing.WeightedWeights.Latency != 0.7 || back.Routing.WeightedWeights.Cost != 0.3 ||
		back.Routing.CooldownMode != "soft-demote" ||
		back.Routing.ChainCooldownModes["gpt-4o"] != "hard" ||
		len(back.Routing.ContextFallbackModels) != 2 ||
		back.Routing.ContextFallbackModels[0] != "gpt-4o-long" ||
		back.Routing.ContextFallbackModels[1] != "claude-long" ||
		back.Routing.ContextReserveTokens != 2048 {
		t.Errorf("routing did not round-trip: %+v", back.Routing)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	clone := cfg.Clone()
	clone.Routing.ChainStrategies["gpt-4o"] = "balanced"
	if cfg.Routing.ChainStrategies["gpt-4o"] != "fastest" {
		t.Error("Clone shared ChainStrategies map with the original")
	}
	clone.Routing.ChainCooldownModes["gpt-4o"] = "soft-demote"
	if cfg.Routing.ChainCooldownModes["gpt-4o"] != "hard" {
		t.Error("Clone shared ChainCooldownModes map with the original")
	}
	clone.Routing.ContextFallbackModels[0] = "changed"
	if cfg.Routing.ContextFallbackModels[0] != "gpt-4o-long" {
		t.Error("Clone shared ContextFallbackModels slice with the original")
	}
	// Refusal chains (CHECKLIST 161): round-trip, deep-copied
	// by Clone, and validated (non-empty model + provider IDs,
	// no duplicate provider in one chain). An explicitly empty
	// list is valid — it means "no refusal routing for this
	// model" — and survives the round-trip as an empty list.
	cfg.Routing.RefusalChains = map[string][]string{
		"m":  {"prov-c", "prov-b"},
		"m2": {},
	}
	raw, err = json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal with refusal chains: %v", err)
	}
	var back2 Config
	if err := json.Unmarshal(raw, &back2); err != nil {
		t.Fatalf("Unmarshal with refusal chains: %v", err)
	}
	if len(back2.Routing.RefusalChains["m"]) != 2 ||
		back2.Routing.RefusalChains["m"][0] != "prov-c" ||
		back2.Routing.RefusalChains["m"][1] != "prov-b" {
		t.Errorf("refusal chains did not round-trip: %+v", back2.Routing.RefusalChains)
	}
	if ids, ok := back2.Routing.RefusalChains["m2"]; !ok || len(ids) != 0 {
		t.Errorf("empty refusal chain did not round-trip: %+v", back2.Routing.RefusalChains)
	}
	if err := back2.Validate(); err != nil {
		t.Errorf("round-tripped refusal chains failed Validate: %v", err)
	}
	clone2 := cfg.Clone()
	clone2.Routing.RefusalChains["m"][0] = "changed"
	if cfg.Routing.RefusalChains["m"][0] != "prov-c" {
		t.Error("Clone shared RefusalChains slice with the original")
	}
	clone2.Routing.RefusalChains["m3"] = []string{"x"}
	if _, ok := cfg.Routing.RefusalChains["m3"]; ok {
		t.Error("Clone shared RefusalChains map with the original")
	}
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"unknown global strategy", func(c *Config) { c.Routing.GlobalStrategy = "smartest" }},
		{"unknown chain strategy", func(c *Config) { c.Routing.ChainStrategies = map[string]string{"m": "smartest"} }},
		{"empty chain model", func(c *Config) { c.Routing.ChainStrategies = map[string]string{"": "fastest"} }},
		{"unknown cooldown mode", func(c *Config) { c.Routing.CooldownMode = "skip" }},
		{"unknown chain cooldown mode", func(c *Config) { c.Routing.ChainCooldownModes = map[string]string{"m": "demote"} }},
		{"empty chain cooldown mode", func(c *Config) { c.Routing.ChainCooldownModes = map[string]string{"m": ""} }},
		{"empty chain cooldown model", func(c *Config) { c.Routing.ChainCooldownModes = map[string]string{"": "hard"} }},
		{"negative deadline budget", func(c *Config) { c.Routing.DeadlineBudgetMs = -1 }},
		{"negative context reserve", func(c *Config) { c.Routing.ContextReserveTokens = -1 }},
		{"empty context fallback model", func(c *Config) { c.Routing.ContextFallbackModels = []string{"gpt-4o-long", ""} }},
		{"empty refusal chain model", func(c *Config) { c.Routing.RefusalChains = map[string][]string{"": {"a"}} }},
		{"empty refusal chain provider", func(c *Config) { c.Routing.RefusalChains = map[string][]string{"m": {"a", ""}} }},
		{"duplicate refusal chain provider", func(c *Config) { c.Routing.RefusalChains = map[string][]string{"m": {"a", "b", "a"}} }},
		{"weight > 1", func(c *Config) { c.Routing.WeightedWeights = &RoutingWeights{Latency: 1.5} }},
		{"weight < 0", func(c *Config) { c.Routing.WeightedWeights = &RoutingWeights{Cost: -0.1} }},
	}
	for _, tc := range bad {
		c := base()
		c.Routing = &RoutingConfig{}
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate passed; want an error naming the field", tc.name)
		}
	}
	// Aliases are accepted ("cheap" is "cheapest"); empty global is valid.
	for _, ok := range []string{"cheap", "fast", "reliable", "latency", ""} {
		c := base()
		c.Routing = &RoutingConfig{}
		c.Routing.GlobalStrategy = ok
		if err := c.Validate(); err != nil {
			t.Errorf("global strategy %q failed Validate: %v", ok, err)
		}
	}
	// Cooldown modes: "" (default = hard), "hard", "soft-demote".
	for _, ok := range []string{"", "hard", "soft-demote"} {
		c := base()
		c.Routing = &RoutingConfig{CooldownMode: ok, ChainCooldownModes: map[string]string{"m": "hard"}}
		if err := c.Validate(); err != nil {
			t.Errorf("cooldown mode %q failed Validate: %v", ok, err)
		}
	}
	// All-zero weights are valid (weighted behaves as balanced).
	c := base()
	c.Routing = &RoutingConfig{}
	c.Routing.WeightedWeights = &RoutingWeights{}
	if err := c.Validate(); err != nil {
		t.Errorf("all-zero weights failed Validate: %v", err)
	}
}

// TestConfigStrategyTreeValidation proves the nestable strategy
// trees (CHECKLIST 177) round-trip through JSON, are deep-copied
// by Clone, and are rejected at load — naming the node path —
// when they carry an unknown strategy, a leaf without a
// provider, a node that is both picker and leaf, or nesting
// past the depth cap.
func TestConfigStrategyTreeValidation(t *testing.T) {
	base := func() *Config { return DefaultConfig(t.TempDir()) }
	tree := &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
		{Strategy: "fastest", Children: []*strategytree.Node{
			{Provider: "prov-a", Model: "m"},
			{Provider: "prov-b", Model: "m"},
		}},
		{Provider: "prov-c", Model: "m"},
	}}
	cfg := base()
	cfg.Routing = &RoutingConfig{
		StrategyTree:  tree,
		StrategyTrees: map[string]*strategytree.Node{"special": {Strategy: "fastest", Children: []*strategytree.Node{{Provider: "prov-a"}}}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Routing.StrategyTree == nil || back.Routing.StrategyTree.Strategy != "cheapest" ||
		len(back.Routing.StrategyTree.Children) != 2 ||
		back.Routing.StrategyTree.Children[0].Children[1].Provider != "prov-b" ||
		back.Routing.StrategyTrees["special"] == nil ||
		back.Routing.StrategyTrees["special"].Children[0].Provider != "prov-a" {
		t.Errorf("strategy trees did not round-trip: %+v", back.Routing)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped trees failed Validate: %v", err)
	}
	clone := cfg.Clone()
	clone.Routing.StrategyTree.Children[0].Children[0].Provider = "changed"
	if cfg.Routing.StrategyTree.Children[0].Children[0].Provider != "prov-a" {
		t.Error("Clone shared StrategyTree nodes with the original")
	}
	clone.Routing.StrategyTrees["special"].Children[0].Provider = "changed"
	if cfg.Routing.StrategyTrees["special"].Children[0].Provider != "prov-a" {
		t.Error("Clone shared StrategyTrees nodes with the original")
	}
	bad := []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"unknown strategy", func(c *Config) {
			c.Routing.StrategyTree = &strategytree.Node{Strategy: "smartest", Children: []*strategytree.Node{{Provider: "a"}}}
		}, "routing.strategy_tree"},
		{"unknown strategy nested", func(c *Config) {
			c.Routing.StrategyTree = &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{
				{Strategy: "nope", Children: []*strategytree.Node{{Provider: "a"}}},
			}}
		}, "root.children[0]"},
		{"leaf without provider", func(c *Config) {
			c.Routing.StrategyTree = &strategytree.Node{Strategy: "cheapest", Children: []*strategytree.Node{{}}}
		}, "root.children[0]"},
		{"picker and leaf", func(c *Config) {
			c.Routing.StrategyTree = &strategytree.Node{Strategy: "cheapest", Provider: "a", Children: []*strategytree.Node{{Provider: "b"}}}
		}, "routing.strategy_tree"},
		{"empty per-chain model", func(c *Config) {
			c.Routing.StrategyTrees = map[string]*strategytree.Node{"": {Provider: "a"}}
		}, "routing.strategy_trees"},
		{"null per-chain tree", func(c *Config) {
			c.Routing.StrategyTrees = map[string]*strategytree.Node{"m": nil}
		}, `routing.strategy_trees["m"]`},
		{"bad per-chain tree", func(c *Config) {
			c.Routing.StrategyTrees = map[string]*strategytree.Node{"m": {Strategy: "weird", Children: []*strategytree.Node{{Provider: "a"}}}}
		}, `routing.strategy_trees["m"]`},
	}
	for _, tc := range bad {
		c := base()
		c.Routing = &RoutingConfig{}
		tc.mut(c)
		err := c.Validate()
		if err == nil {
			t.Errorf("%s: Validate passed; want an error naming the field", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate error = %q, want it to contain %q", tc.name, err, tc.want)
		}
	}
	// A tree nested past the depth cap fails at load too.
	deep := &strategytree.Node{Strategy: "balanced"}
	cur := deep
	for i := 0; i < strategytree.MaxDepth; i++ {
		next := &strategytree.Node{Strategy: "balanced"}
		cur.Children = []*strategytree.Node{next}
		cur = next
	}
	cur.Children = []*strategytree.Node{{Provider: "a"}}
	c := base()
	c.Routing = &RoutingConfig{StrategyTree: deep}
	if err := c.Validate(); err == nil {
		t.Errorf("over-deep tree: Validate passed; want a depth error")
	} else if !strings.Contains(err.Error(), "maximum depth") {
		t.Errorf("over-deep tree: Validate error = %q, want it to name the maximum depth", err)
	}
}

// TestConfigPoolsValidation proves the pools block round-trips
// through JSON, Clone deep-copies it, and invalid values are
// rejected naming the field. The sizing itself is proven in
// internal/provider (TestPoolSizeForRate); this test only pins the
// config plumbing.
func TestConfigPoolsValidation(t *testing.T) {
	base := func() *Config { return DefaultConfig(t.TempDir()) }
	if c := base(); c.Pools != nil {
		t.Fatalf("default config must leave pools unset (nil): %+v", c.Pools)
	}
	cfg := base()
	cfg.Pools = &provider.PoolConfig{ExpectedRPS: 50, ExpectedLatencyMs: 400, MaxIdlePerHost: 0, MinConns: 2, MaxConns: 100}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Pools == nil || back.Pools.ExpectedRPS != 50 || back.Pools.ExpectedLatencyMs != 400 || back.Pools.MaxConns != 100 {
		t.Fatalf("pools did not round-trip: %+v", back.Pools)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	if got := back.Pools.Size(); got != 20 {
		t.Errorf("round-tripped pools Size() = %d, want 20 (50 x 0.4s)", got)
	}
	clone := cfg.Clone()
	clone.Pools.ExpectedRPS = 999
	if cfg.Pools.ExpectedRPS != 50 {
		t.Error("Clone shared the Pools block with the original")
	}
	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"negative rate", func(c *Config) { c.Pools.ExpectedRPS = -1 }},
		{"negative latency", func(c *Config) { c.Pools.ExpectedLatencyMs = -100 }},
		{"negative override", func(c *Config) { c.Pools.MaxIdlePerHost = -1 }},
		{"override too large", func(c *Config) { c.Pools.MaxIdlePerHost = 1001 }},
		{"ceiling below floor", func(c *Config) { c.Pools.MinConns = 10; c.Pools.MaxConns = 5 }},
	}
	for _, tc := range bad {
		c := base()
		c.Pools = &provider.PoolConfig{}
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate passed; want an error naming the field", tc.name)
		}
	}
	// All-zero pools block is valid (means defaults).
	c := base()
	c.Pools = &provider.PoolConfig{}
	if err := c.Validate(); err != nil {
		t.Errorf("all-zero pools failed Validate: %v", err)
	}
}

func TestConfigKeyBudgetsValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		key, _, err := IssueKey("test", []string{"chat"}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := cfg.AddGatewayKey(key); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	keyID := func(c *Config) string { return c.GatewayKeys[0].ID }

	// A valid budget (either policy, including the empty-policy
	// default) validates and round-trips through JSON.
	cfg := base()
	cfg.KeyBudgets = map[string]KeyBudgetConfig{keyID(cfg): {LimitUSD: 5, UnpricedPolicy: "count_as_zero"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("valid key budget failed Validate: %v", err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := back.KeyBudgets[keyID(cfg)]; got.LimitUSD != 5 || got.UnpricedPolicy != "count_as_zero" {
		t.Errorf("key budget did not round-trip: %+v", back.KeyBudgets)
	}
	// Clone deep-copies the map.
	clone := cfg.Clone()
	clone.KeyBudgets[keyID(cfg)] = KeyBudgetConfig{LimitUSD: 99}
	if cfg.KeyBudgets[keyID(cfg)].LimitUSD != 5 {
		t.Error("Clone shared the KeyBudgets map with the original")
	}

	bad := []struct {
		name string
		mut  func(*Config)
	}{
		{"negative limit", func(c *Config) { c.KeyBudgets = map[string]KeyBudgetConfig{keyID(c): {LimitUSD: -1}} }},
		{"unknown policy", func(c *Config) {
			c.KeyBudgets = map[string]KeyBudgetConfig{keyID(c): {LimitUSD: 1, UnpricedPolicy: "bogus"}}
		}},
		{"unknown key", func(c *Config) { c.KeyBudgets = map[string]KeyBudgetConfig{"gk_doesnotexist": {LimitUSD: 1}} }},
		{"empty key id", func(c *Config) { c.KeyBudgets = map[string]KeyBudgetConfig{"": {LimitUSD: 1}} }},
	}
	for _, tc := range bad {
		c := base()
		tc.mut(c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate passed; want an error naming the field", tc.name)
		}
	}
	// Empty policy means the fail_closed default and validates.
	c := base()
	c.KeyBudgets = map[string]KeyBudgetConfig{keyID(c): {LimitUSD: 0}}
	if err := c.Validate(); err != nil {
		t.Errorf("zero-limit default-policy budget failed Validate: %v", err)
	}
}

func TestPriceBookHasPriceForModel(t *testing.T) {
	b := NewPriceBook()
	if b.HasPriceForModel("gpt-4o") {
		t.Error("empty book reports a price")
	}
	if b.HasPriceForModel("") {
		t.Error("empty book reports a price for an empty model")
	}
	// Model-level entry: priced at every provider.
	b.Entries["gpt-4o"] = ModelPrice{}
	if !b.HasPriceForModel("gpt-4o") {
		t.Error("model-level entry not reported as priced")
	}
	// Pair-only entry: priced for that model too.
	b2 := NewPriceBook()
	b2.Pairs[PriceKey{Provider: "p1", Model: "llama"}] = ModelPrice{}
	if !b2.HasPriceForModel("llama") {
		t.Error("pair-only entry not reported as priced")
	}
	if b2.HasPriceForModel("gpt-4o") {
		t.Error("unpriced model reported as priced")
	}
	var nilBook *PriceBook
	if nilBook.HasPriceForModel("gpt-4o") {
		t.Error("nil book reports a price")
	}
}

// TestConfigRoutingGroupsValidation covers the user-named routing
// groups (CHECKLIST 173): round-trip through JSON, deep copy by
// Clone, and load-time validation — an unknown member, an empty
// group, a duplicate member, a bad group name, and a refusal chain
// naming an unknown group all fail naming the offender, while a
// valid group (and a refusal chain naming it) validates.
func TestConfigRoutingGroupsValidation(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.Providers = []ProviderConfig{
			{ID: "prov-a", BaseURL: "http://127.0.0.1:9001"},
			{ID: "prov-b", BaseURL: "http://127.0.0.1:9002"},
		}
		cfg.Routing = &RoutingConfig{
			Groups: map[string][]string{"fast": {"prov-a", "prov-b"}},
		}
		return cfg
	}
	if err := base().Validate(); err != nil {
		t.Errorf("valid groups failed Validate: %v", err)
	}
	// JSON round-trip keeps the group and its order.
	raw, err := json.Marshal(base())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(back.Routing.Groups["fast"]) != 2 || back.Routing.Groups["fast"][0] != "prov-a" || back.Routing.Groups["fast"][1] != "prov-b" {
		t.Errorf("groups did not round-trip: %+v", back.Routing.Groups)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	// Clone deep-copies the group map and its member slices.
	clone := base().Clone()
	clone.Routing.Groups["fast"][0] = "changed"
	if base().Routing.Groups["fast"][0] != "prov-a" {
		t.Error("Clone shared Groups member slice with the original")
	}
	clone.Routing.Groups["slow"] = []string{"prov-b"}
	if len(base().Routing.Groups) != 1 {
		t.Error("Clone shared Groups map with the original")
	}
	// A refusal chain may name the group — and only a group that
	// exists.
	withRefusal := base()
	withRefusal.Routing.RefusalChains = map[string][]string{"m": {"group:fast", "prov-b"}}
	if err := withRefusal.Validate(); err != nil {
		t.Errorf("refusal chain naming a group failed Validate: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*Config)
		want string // substring the error must name
	}{
		{
			name: "unknown member",
			mut: func(c *Config) {
				c.Routing.Groups["fast"] = []string{"prov-a", "prov-ghost"}
			},
			want: "prov-ghost",
		},
		{
			name: "empty group",
			mut: func(c *Config) {
				c.Routing.Groups["fast"] = []string{}
			},
			want: `groups["fast"] is empty`,
		},
		{
			name: "empty member",
			mut: func(c *Config) {
				c.Routing.Groups["fast"] = []string{"prov-a", ""}
			},
			want: `groups["fast"][1] is empty`,
		},
		{
			name: "duplicate member",
			mut: func(c *Config) {
				c.Routing.Groups["fast"] = []string{"prov-a", "prov-a"}
			},
			want: `lists provider "prov-a" twice`,
		},
		{
			name: "name with colon",
			mut: func(c *Config) {
				c.Routing.Groups = map[string][]string{"fa:st": {"prov-a"}}
			},
			want: `"fa:st"`,
		},
		{
			name: "name with slash",
			mut: func(c *Config) {
				c.Routing.Groups = map[string][]string{"fa/st": {"prov-a"}}
			},
			want: `"fa/st"`,
		},
		{
			name: "refusal chain unknown group",
			mut: func(c *Config) {
				c.Routing.RefusalChains = map[string][]string{"m": {"group:nope"}}
			},
			want: `unknown routing group "nope"`,
		},
		{
			name: "refusal chain empty group name",
			mut: func(c *Config) {
				c.Routing.RefusalChains = map[string][]string{"m": {"group:"}}
			},
			want: "group name is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mut(cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate error %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

// TestConfigValidateModelsDev (CHECKLIST 179): the opt-in sync
// block is validated whether or not the sync is enabled, so a typo
// fails at load instead of the day the sync is turned on.
func TestConfigValidateModelsDev(t *testing.T) {
	base := func() *Config {
		cfg := DefaultConfig(t.TempDir())
		cfg.ModelsDev = &ModelsDevConfig{Enabled: true}
		return cfg
	}
	// Enabled with no URL: the default applies, valid.
	if err := base().Validate(); err != nil {
		t.Errorf("enabled with default URL rejected: %v", err)
	}
	// Disabled with a bad URL still fails: validation runs either way.
	cfg := base()
	cfg.ModelsDev.Enabled = false
	cfg.ModelsDev.URL = "ftp://example.com/api.json"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Errorf("non-http URL accepted or wrong error: %v", err)
	}
	cfg = base()
	cfg.ModelsDev.URL = "https://"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "no host") {
		t.Errorf("hostless URL accepted or wrong error: %v", err)
	}
	cfg = base()
	cfg.ModelsDev.URL = "://bad url"
	if err := cfg.Validate(); err == nil {
		t.Errorf("unparseable URL accepted")
	}
	cfg = base()
	cfg.ModelsDev.IntervalMinutes = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "interval_minutes") {
		t.Errorf("negative interval accepted or wrong error: %v", err)
	}
	cfg = base()
	cfg.ModelsDev.TimeoutSeconds = 121
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "timeout_seconds") {
		t.Errorf("timeout > 120 accepted or wrong error: %v", err)
	}
	cfg = base()
	cfg.ModelsDev.TimeoutSeconds = 120
	if err := cfg.Validate(); err != nil {
		t.Errorf("timeout 120 rejected: %v", err)
	}
	// Defaults: empty URL -> models.dev, 0 timeout -> 15s.
	md := &ModelsDevConfig{}
	if md.EffectiveURL() != DefaultModelsDevURL {
		t.Errorf("EffectiveURL = %q, want %q", md.EffectiveURL(), DefaultModelsDevURL)
	}
	if md.EffectiveTimeoutSeconds() != DefaultModelsDevTimeoutSeconds {
		t.Errorf("EffectiveTimeoutSeconds = %d, want %d", md.EffectiveTimeoutSeconds(), DefaultModelsDevTimeoutSeconds)
	}
	// Clone deep-copies the block.
	cfg = base()
	cfg.ModelsDev.URL = "https://example.com/api.json"
	clone := cfg.Clone()
	clone.ModelsDev.URL = "https://other.example.com/api.json"
	if cfg.ModelsDev.URL != "https://example.com/api.json" {
		t.Error("clone mutated the original models_dev URL")
	}
	// Round-trip through JSON keeps the block.
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ModelsDev == nil || !back.ModelsDev.Enabled || back.ModelsDev.URL != "https://example.com/api.json" {
		t.Errorf("round-tripped models_dev = %+v", back.ModelsDev)
	}
}

// TestConfigResponsesSessionTokenCap proves the Responses session
// cap is wired through config (CHECKLIST 263–264): unset means
// nil (counting-only), a value round-trips through JSON and
// validates, and a negative cap is rejected at load naming the
// field — 0 already means counting-only, so a negative value
// can only be a typo.
func TestConfigResponsesSessionTokenCap(t *testing.T) {
	if c := DefaultConfig(t.TempDir()); c.Responses != nil {
		t.Fatalf("default config must leave responses unset (nil): %+v", c.Responses)
	}
	cfg := DefaultConfig(t.TempDir())
	cfg.Responses = &ResponsesConfig{SessionTokenCap: 1000}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Responses == nil || back.Responses.SessionTokenCap != 1000 {
		t.Fatalf("responses did not round-trip: %+v", back.Responses)
	}
	// The key is a KNOWN field: it must not also land in
	// Extensions, or MarshalJSON would re-emit it a second
	// time on save (duplicate top-level key).
	if _, ok := back.Extensions["responses"]; ok {
		t.Fatalf("responses landed in Extensions: %+v", back.Extensions)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("config with session_token_cap=1000 failed Validate: %v", err)
	}
	cfg.Responses.SessionTokenCap = -1
	if err := cfg.Validate(); err == nil {
		t.Error("Validate should reject a negative responses.session_token_cap")
	} else if !strings.Contains(err.Error(), "responses.session_token_cap") {
		t.Errorf("negative-cap error = %v, want it to name responses.session_token_cap", err)
	}
	cfg.Responses.SessionTokenCap = 0
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config with session_token_cap=0 failed Validate: %v", err)
	}
}

// TestConfigRoutingStrategyNamesRoundTrip — M1.10/M1.11 acceptance:
// config must accept every strategy the strategy package declares
// (see strategy.All) plus the aliases, as both the global strategy
// and per-chain overrides, through a JSON round-trip; unknown names
// are still rejected at load.
func TestConfigRoutingStrategyNamesRoundTrip(t *testing.T) {
	declared := []string{"priority", "balanced", "cheapest", "fastest", "most-reliable", "least-latency", "weighted", "custom-weights", "code", "round-robin", "random", "strict-random", "fill-first", "least-used", "p2c", "quota-weighted", "headroom", "expiry-first", "reset-aware", "reset-window", "lkgp", "context-optimized", "cache-optimized", "context-relay", "cost-optimized", "ship-fast", "cost-saver", "quality-first", "offline-friendly", "reliability-first", "chaos-mode"}
	aliases := []string{"cheap", "fast", "reliable", "latency", "eco", "custom"}
	for _, name := range append(append([]string{}, declared...), aliases...) {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{
			GlobalStrategy:  name,
			ChainStrategies: map[string]string{"gpt-4o": name},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("strategy %q failed Validate: %v", name, err)
			continue
		}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("strategy %q: Marshal: %v", name, err)
		}
		var back Config
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("strategy %q: Unmarshal: %v", name, err)
		}
		if back.Routing.GlobalStrategy != name || back.Routing.ChainStrategies["gpt-4o"] != name {
			t.Errorf("strategy %q did not round-trip: global=%q chain=%q", name, back.Routing.GlobalStrategy, back.Routing.ChainStrategies["gpt-4o"])
		}
		if err := back.Validate(); err != nil {
			t.Errorf("strategy %q round-tripped config failed Validate: %v", name, err)
		}
	}
	// Unknown names are still load errors, naming the field.
	for _, bad := range []string{"smartest", "codex"} {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{GlobalStrategy: bad}
		if err := cfg.Validate(); err == nil {
			t.Errorf("global strategy %q passed Validate; want an error", bad)
		}
		cfg.Routing = &RoutingConfig{ChainStrategies: map[string]string{"m": bad}}
		if err := cfg.Validate(); err == nil {
			t.Errorf("chain strategy %q passed Validate; want an error", bad)
		}
	}
}

// M3 Area 1 (PARITY row 388): routing.account_fallback_strategy
// / account_fallback_strategies round-trip through the real
// loader, per-provider values survive alongside the global one,
// and unknown names (including the blocked "expiry-first") fail
// Validate naming the value.
func TestConfigAccountFallbackRoundTrip(t *testing.T) {
	valid := []string{"priority", "fill-first", "round-robin", "p2c", "random", "least-used", "cost-optimized", "strict-random", "weighted"}
	for _, name := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{
			AccountFallbackStrategy:   name,
			AccountFallbackStrategies: map[string]string{"prov": name},
		}
		if err := cfg.Validate(); err != nil {
			t.Errorf("account fallback %q failed Validate: %v", name, err)
			continue
		}
		loaded, err := Load(writeTempConfig(t, cfg))
		if err != nil {
			t.Errorf("account fallback %q failed Load: %v", name, err)
			continue
		}
		if loaded.Routing.AccountFallbackStrategy != name || loaded.Routing.AccountFallbackStrategies["prov"] != name {
			t.Errorf("account fallback %q did not survive Load: global=%q prov=%q", name, loaded.Routing.AccountFallbackStrategy, loaded.Routing.AccountFallbackStrategies["prov"])
		}
	}
	// Global and per-provider may differ: both survive Load
	// (the per-provider override wins at request time — that
	// resolution is the engine's, covered by
	// TestAccountFallbackStrategyFor in internal/router).
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		AccountFallbackStrategy:   "round-robin",
		AccountFallbackStrategies: map[string]string{"prov": "least-used"},
	}
	loaded, err := Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("mixed account fallback failed Load: %v", err)
	}
	if loaded.Routing.AccountFallbackStrategy != "round-robin" || loaded.Routing.AccountFallbackStrategies["prov"] != "least-used" {
		t.Errorf("mixed account fallback did not survive Load: global=%q prov=%q", loaded.Routing.AccountFallbackStrategy, loaded.Routing.AccountFallbackStrategies["prov"])
	}
	// Unknown names fail Validate with the value named — at
	// both levels. "expiry-first" is rejected here too: no
	// per-key quota-expiry source exists (design §D2).
	for _, bad := range []string{"expiry-first", "EXPIRY-FIRST", "bogus"} {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{AccountFallbackStrategy: bad}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("global account fallback %q passed Validate; want an error", bad)
		} else if !strings.Contains(err.Error(), bad) {
			t.Errorf("global account fallback %q error %v does not name the value", bad, err)
		}
		cfg.Routing = &RoutingConfig{AccountFallbackStrategies: map[string]string{"prov": bad}}
		err = cfg.Validate()
		if err == nil {
			t.Errorf("per-provider account fallback %q passed Validate; want an error", bad)
		} else if !strings.Contains(err.Error(), bad) {
			t.Errorf("per-provider account fallback %q error %v does not name the value", bad, err)
		}
	}
}

// M3 Area 1 (PARITY row 394): routing.sla round-trips through
// the real loader — a full block survives Load, an absent block
// stays nil (defaults), and a partial block keeps only the set
// fields as non-nil pointers (the rest stay nil = default).
func TestConfigSLARoundTrip(t *testing.T) {
	f64 := func(v float64) *float64 { return &v }
	b := func(v bool) *bool { return &v }
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		SLA: &SLAConfig{
			TargetP95Ms:     f64(500),
			MaxErrorRate:    f64(0.01),
			MaxCostPer1M:    f64(5),
			HardConstraints: b(true),
		},
	}
	loaded, err := Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("sla block failed Load: %v", err)
	}
	sla := loaded.Routing.SLA
	if sla == nil || sla.TargetP95Ms == nil || *sla.TargetP95Ms != 500 ||
		sla.MaxErrorRate == nil || *sla.MaxErrorRate != 0.01 ||
		sla.MaxCostPer1M == nil || *sla.MaxCostPer1M != 5 ||
		sla.HardConstraints == nil || *sla.HardConstraints != true {
		t.Errorf("sla block did not survive Load: %+v", sla)
	}
	// Absent block stays nil: the engine defaults apply.
	cfg = DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{}
	loaded, err = Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("absent sla block failed Load: %v", err)
	}
	if loaded.Routing.SLA != nil {
		t.Errorf("absent sla block loaded as %+v, want nil", loaded.Routing.SLA)
	}
	// Partial block: only the set field is non-nil.
	cfg = DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{SLA: &SLAConfig{MaxErrorRate: f64(0.2)}}
	loaded, err = Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("partial sla block failed Load: %v", err)
	}
	sla = loaded.Routing.SLA
	if sla == nil || sla.MaxErrorRate == nil || *sla.MaxErrorRate != 0.2 ||
		sla.TargetP95Ms != nil || sla.MaxCostPer1M != nil || sla.HardConstraints != nil {
		t.Errorf("partial sla block loaded as %+v, want only max_error_rate=0.2 set", sla)
	}
}

// M3 Area 1 (PARITY row 394): an explicit max_error_rate of 0
// in a real config FILE loads as a non-nil pointer to 0 —
// zero-tolerance — and must never be read back as the 0.05
// default or as "unset". This is the file-level half of the
// nil-vs-0 distinction; the engine half is
// TestSLAConfiguredPolicyReachesHookContext in internal/router
// (a 0 there must reach the hook as 0, not 0.05).
func TestConfigSLAExplicitZeroIsZeroTolerance(t *testing.T) {
	raw := `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"sla": {"max_error_rate": 0}}}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("sla max_error_rate 0 failed Load: %v", err)
	}
	sla := loaded.Routing.SLA
	if sla == nil || sla.MaxErrorRate == nil {
		t.Fatalf("sla max_error_rate 0 loaded as %+v, want a non-nil pointer to 0", sla)
	}
	if *sla.MaxErrorRate != 0 {
		t.Errorf("sla max_error_rate loaded as %v, want exactly 0 (zero-tolerance, not the 0.05 default)", *sla.MaxErrorRate)
	}
}

// M3 Area 1 (PARITY row 394): routing.sla validation. A
// non-positive target, an error rate outside [0, 1], and a
// negative cost cap fail Validate naming the field; an explicit
// 0 error rate (zero-tolerance) and a 0 cost cap (no cap) pass.
func TestConfigSLAValidation(t *testing.T) {
	f64 := func(v float64) *float64 { return &v }
	valid := []*SLAConfig{
		{MaxErrorRate: f64(0)},
		{MaxErrorRate: f64(1)},
		{TargetP95Ms: f64(500), MaxErrorRate: f64(0.05), MaxCostPer1M: f64(0)},
	}
	for i, sla := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{SLA: sla}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid sla block %d (%+v) failed Validate: %v", i, sla, err)
		}
	}
	bad := []struct {
		sla  *SLAConfig
		want string
	}{
		{&SLAConfig{TargetP95Ms: f64(0)}, "target_p95_ms"},
		{&SLAConfig{TargetP95Ms: f64(-100)}, "target_p95_ms"},
		{&SLAConfig{MaxErrorRate: f64(-0.1)}, "max_error_rate"},
		{&SLAConfig{MaxErrorRate: f64(1.5)}, "max_error_rate"},
		{&SLAConfig{MaxCostPer1M: f64(-1)}, "max_cost_per_1m_tokens"},
	}
	for _, tc := range bad {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{SLA: tc.sla}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("sla block %+v passed Validate; want an error", tc.sla)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("sla block %+v error %v does not name %q", tc.sla, err, tc.want)
		}
	}
}

// M3 Area 1 (PARITY row 396): routing.nadir round-trip
// through a real config file — full block, partial block
// (endpoint only, timeout defaults), and absent block
// (unconfigured, nil). A Clone of a loaded config must
// keep its own nadir block: mutating the clone must not
// move the live config.
func TestConfigNadirRoundTrip(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{
		Nadir: &NadirConfig{Endpoint: "https://api.getnadir.com", TimeoutMs: 5000},
	}
	loaded, err := Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("nadir block failed Load: %v", err)
	}
	nadir := loaded.Routing.Nadir
	if nadir == nil || nadir.Endpoint != "https://api.getnadir.com" || nadir.TimeoutMs != 5000 {
		t.Errorf("nadir block did not survive Load: %+v", nadir)
	}
	// Clone isolation: the clone's block is its own copy.
	clone := loaded.Clone()
	clone.Routing.Nadir.Endpoint = "https://other.example"
	clone.Routing.Nadir.TimeoutMs = 1000
	if loaded.Routing.Nadir.Endpoint != "https://api.getnadir.com" || loaded.Routing.Nadir.TimeoutMs != 5000 {
		t.Errorf("mutating a Clone moved the live nadir block: %+v", loaded.Routing.Nadir)
	}
	// Absent block stays nil: the engine defaults apply
	// (fail-closed, no external call).
	cfg = DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{}
	loaded, err = Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("absent nadir block failed Load: %v", err)
	}
	if loaded.Routing.Nadir != nil {
		t.Errorf("absent nadir block loaded as %+v, want nil", loaded.Routing.Nadir)
	}
	// Partial block: only the set field is non-zero; a 0
	// timeout means the 2000 ms engine default, not an error.
	cfg = DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{Nadir: &NadirConfig{Endpoint: "https://nadir.example/v1/"}}
	loaded, err = Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("partial nadir block failed Load: %v", err)
	}
	nadir = loaded.Routing.Nadir
	if nadir == nil || nadir.Endpoint != "https://nadir.example/v1/" || nadir.TimeoutMs != 0 {
		t.Errorf("partial nadir block loaded as %+v, want endpoint only, timeout 0", nadir)
	}
	// An endpoint with a pasted /v1 and surrounding spaces
	// loads as written; the engine strips the /v1 at use.
	// An empty endpoint in a present block is unconfigured,
	// not an error.
	cfg = DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{Nadir: &NadirConfig{}}
	loaded, err = Load(writeTempConfig(t, cfg))
	if err != nil {
		t.Fatalf("empty nadir block failed Load: %v", err)
	}
	if loaded.Routing.Nadir == nil || loaded.Routing.Nadir.Endpoint != "" {
		t.Errorf("empty nadir block loaded as %+v, want a present block with an empty endpoint", loaded.Routing.Nadir)
	}
	// Literal file: the block is found under the "nadir"
	// key in raw JSON, not just through a Go-struct
	// marshal — so this FAILS if the json tag is ever
	// renamed or the field dropped (a struct round-trip
	// alone would not catch that).
	raw := `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"nadir": {"endpoint": "https://raw.example", "timeout_ms": 7000}}}`
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(path)
	if err != nil {
		t.Fatalf("literal nadir block failed Load: %v", err)
	}
	nadir = loaded.Routing.Nadir
	if nadir == nil || nadir.Endpoint != "https://raw.example" || nadir.TimeoutMs != 7000 {
		t.Errorf("literal nadir block loaded as %+v, want endpoint https://raw.example timeout 7000", nadir)
	}
}

// TestConfigNadirCooldownLoadFillIn (G5): Load fills the
// nadir failure cooldown with the 30 s default when the
// nadir block exists but omits cooldown_ms; an explicit
// value is preserved; and an absent nadir block stays nil
// (fail-closed — auto/nadir never calls any external API).
func TestConfigNadirCooldownLoadFillIn(t *testing.T) {
	loadRaw := func(t *testing.T, raw string) *Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("Load failed: %v", err)
		}
		return loaded
	}
	// Omitted -> 30000 ms default.
	loaded := loadRaw(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"nadir": {"endpoint": "https://nadir.example", "timeout_ms": 7000}}}`)
	if loaded.Routing == nil || loaded.Routing.Nadir == nil || loaded.Routing.Nadir.CooldownMs != 30000 {
		t.Errorf("omitted cooldown_ms loaded as %+v, want 30000", loaded.Routing.Nadir)
	}
	// Explicit -> preserved.
	loaded = loadRaw(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data", "routing": {"nadir": {"endpoint": "https://nadir.example", "cooldown_ms": 5000}}}`)
	if loaded.Routing == nil || loaded.Routing.Nadir == nil || loaded.Routing.Nadir.CooldownMs != 5000 {
		t.Errorf("explicit cooldown_ms loaded as %+v, want 5000", loaded.Routing.Nadir)
	}
	// No nadir block -> nil (unconfigured, fail-closed).
	loaded = loadRaw(t, `{"version": 1, "listen": "127.0.0.1:20128", "data_dir": "/tmp/data"}`)
	if loaded.Routing == nil || loaded.Routing.Nadir != nil {
		t.Errorf("absent nadir block loaded as %+v, want nil", loaded.Routing.Nadir)
	}
}

// M3 Area 1 (PARITY row 396): routing.nadir validation. A
// non-http(s) endpoint, a relative URL, a timeout
// outside [0, 30000], and a negative cooldown fail Validate
// naming the field; an empty endpoint, a 0 timeout (default),
// the 30000 cap, a trailing-/v1 endpoint, a 0 cooldown
// (default), and a cooldown above 30000 (no upper cap — an
// operator may cool a flaky endpoint down longer) pass.
func TestConfigNadirValidation(t *testing.T) {
	valid := []*NadirConfig{
		nil,
		{},
		{Endpoint: ""},
		{Endpoint: "https://api.getnadir.com", TimeoutMs: 0},
		{Endpoint: "https://api.getnadir.com", TimeoutMs: 30000},
		{Endpoint: "http://localhost:8080/v1/", TimeoutMs: 500},
		{Endpoint: "  https://nadir.example  ", TimeoutMs: 2000},
		{Endpoint: "https://api.getnadir.com", CooldownMs: 0},
		{Endpoint: "https://api.getnadir.com", CooldownMs: 60000},
	}
	for i, nadir := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{Nadir: nadir}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid nadir block %d (%+v) failed Validate: %v", i, nadir, err)
		}
	}
	bad := []struct {
		nadir *NadirConfig
		want  string
	}{
		{&NadirConfig{Endpoint: "ftp://not-http.example"}, "nadir.endpoint"},
		{&NadirConfig{Endpoint: "not-a-url"}, "nadir.endpoint"},
		{&NadirConfig{Endpoint: "/v1/bucket"}, "nadir.endpoint"},
		{&NadirConfig{Endpoint: "https://"}, "nadir.endpoint"},
		{&NadirConfig{Endpoint: "https://api.getnadir.com", TimeoutMs: -1}, "nadir.timeout_ms"},
		{&NadirConfig{Endpoint: "https://api.getnadir.com", TimeoutMs: 30001}, "nadir.timeout_ms"},
		{&NadirConfig{Endpoint: "https://api.getnadir.com", TimeoutMs: 60000}, "nadir.timeout_ms"},
		{&NadirConfig{Endpoint: "https://api.getnadir.com", CooldownMs: -1}, "nadir.cooldown_ms"},
	}
	for _, tc := range bad {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{Nadir: tc.nadir}
		err := cfg.Validate()
		if err == nil {
			t.Errorf("nadir block %+v passed Validate; want an error", tc.nadir)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("nadir block %+v error %v does not name %q", tc.nadir, err, tc.want)
		}
	}
}

// M8.20 F3: routing.media_timeouts_ms validation. Valid values
// pass (including 0 = default and an absent map); unknown
// endpoint keys, negative values and values above 1 h fail at
// load, naming the field — so a typo'd endpoint or a
// milliseconds/seconds unit slip can never silently do nothing.
func TestConfigMediaTimeoutsValidation(t *testing.T) {
	valid := []map[string]int64{
		nil,
		{},
		{"music": 600000},
		{"search": 30000, "ocr": 30000, "video": 30000, "music": 600000},
		{"music": 0},       // 0 = default
		{"music": 3600000}, // exactly 1 h, the cap
		{"video": 120000},
	}
	for i, ms := range valid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{MediaTimeoutsMs: ms}
		if err := cfg.Validate(); err != nil {
			t.Errorf("valid case %d (%v) failed Validate: %v", i, ms, err)
		}
	}
	invalid := []map[string]int64{
		{"chat": 1000},       // unknown endpoint
		{"Music": 600000},    // keys are lowercase endpoint names
		{"music": -1},        // negative
		{"music": 3600001},   // above the 1 h cap
		{"music": 600000000}, // seconds mistaken for ms
		{"search": -60000},
	}
	for i, ms := range invalid {
		cfg := DefaultConfig(t.TempDir())
		cfg.Routing = &RoutingConfig{MediaTimeoutsMs: ms}
		if err := cfg.Validate(); err == nil {
			t.Errorf("invalid case %d (%v) passed Validate; want an error", i, ms)
		}
	}
	// Round-trip + deep copy: the map survives JSON and Clone
	// does not share it with the original.
	cfg := DefaultConfig(t.TempDir())
	cfg.Routing = &RoutingConfig{MediaTimeoutsMs: map[string]int64{"music": 600000, "search": 30000}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Config
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.Routing.MediaTimeoutsMs["music"] != 600000 || back.Routing.MediaTimeoutsMs["search"] != 30000 {
		t.Errorf("media timeouts did not round-trip: %v", back.Routing.MediaTimeoutsMs)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	clone := cfg.Clone()
	clone.Routing.MediaTimeoutsMs["music"] = 1000
	if cfg.Routing.MediaTimeoutsMs["music"] != 600000 {
		t.Error("Clone shared MediaTimeoutsMs map with the original")
	}
	// RoutingSnapshot carries the map too (the server reads it
	// through the snapshot, never the live pointer).
	snap := cfg.RoutingSnapshot()
	if snap.MediaTimeoutsMs["music"] != 600000 {
		t.Errorf("RoutingSnapshot lost MediaTimeoutsMs: %v", snap.MediaTimeoutsMs)
	}
	snap.MediaTimeoutsMs["music"] = 1000
	if cfg.Routing.MediaTimeoutsMs["music"] != 600000 {
		t.Error("RoutingSnapshot shared MediaTimeoutsMs map with the live config")
	}
}
