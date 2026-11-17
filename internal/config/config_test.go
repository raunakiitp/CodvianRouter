package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if len(decoded) != 9 {
		t.Errorf("encoded config has %d top-level keys, want 9: %v", len(decoded), decoded)
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
		Threshold:    0.6,
		Thresholds:   map[string]float64{"auto/fast": 0.7},
		DefaultTier:  "coding",
		DefaultModel: "strong-model",
		TierModels:   map[string][]string{"coding": {"coder-model"}, "chat": {"chat-model"}},
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
		len(back.AutoRouter.TierModels["coding"]) != 1 {
		t.Errorf("auto_router did not round-trip: %+v", back.AutoRouter)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round-tripped config failed Validate: %v", err)
	}
	// Clone deep-copies the auto-router maps.
	clone := cfg.Clone()
	clone.AutoRouter.Thresholds["auto/fast"] = 0.1
	clone.AutoRouter.TierModels["coding"][0] = "other-model"
	if cfg.AutoRouter.Thresholds["auto/fast"] != 0.7 {
		t.Error("Clone shared Thresholds map with the original")
	}
	if cfg.AutoRouter.TierModels["coding"][0] != "coder-model" {
		t.Error("Clone shared TierModels slices with the original")
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
