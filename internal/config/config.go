// Package config owns the versioned, JSON-backed configuration for
// codvian-router: loading, validation, atomic saving, and defaults.
//
// Config file format (version 1):
//
//	{
//	  "version": 1,
//	  "listen": "127.0.0.1:20128",
//	  "data_dir": "/home/user/.local/share/codvian-router",
//	  "gateway_keys": [ ... ],   // see gatewaykey.go; only hashes are stored
//	  "providers": [ ... ],      // see ProviderConfig below
//	  "budget_usd": 0,           // 0 = no global cap set
//	  "fail_closed": true,       // refuse work instead of bypassing limits
//	  "telemetry_enabled": false // no telemetry ever unless explicitly true
//	}
//
// The router is local-only by design: Listen must be a loopback address
// (127.0.0.1 or ::1). Validate enforces this.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// CurrentVersion is the only config version this build understands.
const CurrentVersion = 1

// DefaultListen is the default bind address. Localhost only, never 0.0.0.0.
const DefaultListen = "127.0.0.1:20128"

// DefaultPort is the port portion of DefaultListen.
const DefaultPort = 20128

// Config is the on-disk router configuration (version 1).
//
// Extensions holds top-level JSON fields this build does not recognise,
// keyed by their JSON name. They are preserved verbatim across Load/Save
// round-trips (see MarshalJSON/UnmarshalJSON) so a config written by a
// newer version does not silently lose fields it does not understand.
// Extensions is tagged json:"-" so it never appears as a field of its
// own in the normal field layout; its entries are re-emitted as
// top-level keys. Callers must not put a known field name in Extensions:
// known fields always win on marshal.
type Config struct {
	Version          int              `json:"version"`
	Listen           string           `json:"listen"`
	DataDir          string           `json:"data_dir"`
	GatewayKeys      []GatewayKey     `json:"gateway_keys"`
	Providers        []ProviderConfig `json:"providers"`
	BudgetUSD        float64          `json:"budget_usd"`
	FailClosed       bool             `json:"fail_closed"`
	TelemetryEnabled bool             `json:"telemetry_enabled"`

	Extensions map[string]json.RawMessage `json:"-"`
}

// knownConfigFields is the set of top-level JSON keys owned by Config v1.
// Anything else in the file is an unknown field and goes to Extensions.
var knownConfigFields = map[string]bool{
	"version":           true,
	"listen":            true,
	"data_dir":          true,
	"gateway_keys":      true,
	"providers":         true,
	"budget_usd":        true,
	"fail_closed":       true,
	"telemetry_enabled": true,
}

// configAlias is Config without its MarshalJSON/UnmarshalJSON methods, so
// the custom methods can delegate the known fields to encoding/json
// without recursing.
type configAlias Config

// MarshalJSON encodes the known v1 fields exactly as before, then
// re-emits every Extensions entry as a top-level key with its raw JSON
// bytes verbatim. Extension keys are sorted so output is deterministic;
// a key that collides with a known field is dropped (known fields win).
// A nil *Config encodes as JSON null.
func (c *Config) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	known, err := json.Marshal((*configAlias)(c))
	if err != nil {
		return nil, err
	}
	if len(c.Extensions) == 0 {
		return known, nil
	}
	keys := make([]string, 0, len(c.Extensions))
	for k, v := range c.Extensions {
		if knownConfigFields[k] || v == nil {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return known, nil
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	// known is a JSON object; splice the extension entries in before
	// the closing brace so each raw value is emitted byte-for-byte.
	buf.Write(known[:len(known)-1])
	for _, k := range keys {
		keyJSON, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.WriteByte(',')
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(c.Extensions[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// UnmarshalJSON decodes the known v1 fields as before and stores every
// other top-level field, raw bytes included, in Extensions. Unknown
// fields are never interpreted or validated, only preserved. When the
// input has no unknown fields, Extensions is left nil.
func (c *Config) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var alias configAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	*c = Config(alias)
	c.Extensions = nil
	for k, v := range raw {
		if knownConfigFields[k] {
			continue
		}
		if c.Extensions == nil {
			c.Extensions = make(map[string]json.RawMessage, len(raw))
		}
		cp := make(json.RawMessage, len(v))
		copy(cp, v)
		c.Extensions[k] = cp
	}
	return nil
}

// ProviderConfig is the persisted shape of one upstream provider entry in
// the config file. It is an alias for provider.ProviderConfig — the single
// source of truth defined in internal/provider/provider.go — so the config
// file and the provider registry can never drift apart (same field names
// and JSON tags, same Models element type).
//
// The API key is never stored in plaintext: APIKeyEncrypted holds the
// sealed value produced by internal/crypto (Vault.Seal). The provider
// package unseals it at startup; see internal/crypto/README.md for the
// exact at-rest encryption method.
type ProviderConfig = provider.ProviderConfig

// DefaultDataDir returns the default data directory
// (~/.local/share/codvian-router).
func DefaultDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate home directory: %w", err)
	}
	return filepath.Join(home, ".local", "share", "codvian-router"), nil
}

// DefaultConfigPath returns the default config file path
// (~/.config/codvian-router/config.json).
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: cannot locate user config directory: %w", err)
	}
	return filepath.Join(dir, "codvian-router", "config.json"), nil
}

// DefaultConfig returns a fresh version-1 config with safe defaults:
// localhost-only listen, auth-relevant fail-closed mode on, telemetry off,
// no global budget cap (0), and no providers or gateway keys yet.
func DefaultConfig(dataDir string) *Config {
	if dataDir == "" {
		if d, err := DefaultDataDir(); err == nil {
			dataDir = d
		}
	}
	return &Config{
		Version:          CurrentVersion,
		Listen:           DefaultListen,
		DataDir:          dataDir,
		GatewayKeys:      []GatewayKey{},
		Providers:        []ProviderConfig{},
		BudgetUSD:        0,
		FailClosed:       true,
		TelemetryEnabled: false,
	}
}

// Validate checks the config for values this build can safely run with.
// It enforces: known version, parseable loopback-only listen address,
// non-negative budget, and a non-empty data directory.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config: nil config")
	}
	if c.Version != CurrentVersion {
		return fmt.Errorf("config: unsupported version %d (want %d)", c.Version, CurrentVersion)
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("config: invalid listen address %q: %w", c.Listen, err)
	}
	if port == "" {
		return fmt.Errorf("config: listen address %q has no port", c.Listen)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("config: listen host %q must be a loopback address (127.0.0.1 or ::1); the router is local-only", host)
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if c.BudgetUSD < 0 {
		return fmt.Errorf("config: budget_usd must not be negative (got %v)", c.BudgetUSD)
	}
	for i, p := range c.Providers {
		if p.ID == "" {
			return fmt.Errorf("config: providers[%d] has an empty id", i)
		}
		// A catalog-linked provider may leave base_url empty: the
		// registry resolves it from the catalog entry at load. A
		// fully-custom provider must name its own base URL.
		if p.BaseURL == "" && p.CatalogID == "" {
			return fmt.Errorf("config: provider %q has an empty base_url", p.ID)
		}
	}
	return nil
}

// Clone returns a deep copy of the config, including Extensions and all
// slices (gateway-key scopes and provider models included), so a watcher
// or other goroutine can hold and mutate its own copy without affecting
// the original. A nil *Config clones to nil.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	if c.GatewayKeys != nil {
		out.GatewayKeys = make([]GatewayKey, len(c.GatewayKeys))
		for i, k := range c.GatewayKeys {
			if k.Scopes != nil {
				k.Scopes = append([]string(nil), k.Scopes...)
			}
			out.GatewayKeys[i] = k
		}
	}
	if c.Providers != nil {
		out.Providers = make([]ProviderConfig, len(c.Providers))
		for i, p := range c.Providers {
			if p.Models != nil {
				p.Models = append([]provider.Model(nil), p.Models...)
			}
			out.Providers[i] = p
		}
	}
	if c.Extensions != nil {
		out.Extensions = make(map[string]json.RawMessage, len(c.Extensions))
		for k, v := range c.Extensions {
			cp := make(json.RawMessage, len(v))
			copy(cp, v)
			out.Extensions[k] = cp
		}
	}
	return &out
}

// ValidateFile loads the config file at path, runs Config.Validate, and
// additionally rejects duplicate gateway key IDs, duplicate provider IDs,
// and duplicate provider base URLs (after normalization: surrounding
// whitespace and trailing slashes removed, scheme and host lowercased,
// default ports stripped). The checks inherited from Validate cover:
// version != 1, a listen address that is not loopback, a negative
// budget_usd, and any provider with an empty id or base_url. Every error
// names the offending field and value.
//
// Limitation: base-URL normalization is syntactic only. URLs that differ
// in ways normalization cannot see (different hostnames resolving to the
// same server, percent-encoding, query strings) are not treated as
// duplicates, and an unparseable base URL is compared as a lowercased,
// trimmed string.
func ValidateFile(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: cannot read %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("config: invalid JSON in %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	seenKeys := make(map[string]bool, len(cfg.GatewayKeys))
	for _, k := range cfg.GatewayKeys {
		if seenKeys[k.ID] {
			return fmt.Errorf("config: duplicate gateway key id %q", k.ID)
		}
		seenKeys[k.ID] = true
	}
	seenProviders := make(map[string]bool, len(cfg.Providers))
	seenURLs := make(map[string]string, len(cfg.Providers))
	for _, p := range cfg.Providers {
		if seenProviders[p.ID] {
			return fmt.Errorf("config: duplicate provider id %q", p.ID)
		}
		seenProviders[p.ID] = true
		if p.BaseURL == "" {
			continue // catalog-resolved at load; nothing to compare yet
		}
		norm := normalizeBaseURL(p.BaseURL)
		if first, ok := seenURLs[norm]; ok {
			return fmt.Errorf("config: duplicate provider base_url %q (provider %q): same normalized URL as provider %q", p.BaseURL, p.ID, first)
		}
		seenURLs[norm] = p.ID
	}
	return nil
}

// normalizeBaseURL canonicalizes a provider base URL for duplicate
// detection: trims surrounding whitespace and trailing slashes,
// lowercases the scheme and host, and strips a default port (:80 for
// http, :443 for https). Anything it cannot parse as a URL with a scheme
// and host is returned lowercased and trimmed instead.
func normalizeBaseURL(raw string) string {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return strings.ToLower(s)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)
	if (u.Scheme == "https" && strings.HasSuffix(host, ":443")) ||
		(u.Scheme == "http" && strings.HasSuffix(host, ":80")) {
		host = host[:strings.LastIndex(host, ":")]
	}
	u.Host = host
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// EnsureDataDir creates the data directory (0700) if it does not exist.
func (c *Config) EnsureDataDir() error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return fmt.Errorf("config: cannot create data_dir %q: %w", c.DataDir, err)
	}
	return nil
}

// Load reads and validates the config file at path.
// It returns an error (wrapping os.ErrNotExist) when the file is missing;
// callers that want creation should use LoadOrCreate.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: cannot read %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("config: invalid JSON in %q: %w", path, err)
	}
	// Fill in zero-value slices so callers never see nil.
	if cfg.GatewayKeys == nil {
		cfg.GatewayKeys = []GatewayKey{}
	}
	if cfg.Providers == nil {
		cfg.Providers = []ProviderConfig{}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// LoadOrCreate loads the config at path, creating a default one (and its
// parent directory) when the file does not exist yet. The created file is
// written with 0600 permissions via Save.
func LoadOrCreate(path string) (*Config, error) {
	cfg, err := Load(path)
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	dataDir, derr := DefaultDataDir()
	if derr != nil {
		return nil, derr
	}
	cfg = DefaultConfig(dataDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("config: cannot create config directory for %q: %w", path, err)
	}
	if err := cfg.Save(path); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config to path atomically: it writes a temp file in the
// same directory (0600), fsyncs it, then renames over path, so a crash
// mid-write can never leave a half-written config behind. It also creates
// the parent directory when needed.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: cannot encode config: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: cannot create directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: cannot create temp file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup of the temp file on any failure path.
	defer func() {
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot write temp config: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot set temp config permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: cannot sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: cannot close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("config: cannot replace %q: %w", path, err)
	}
	return nil
}

// SetListenPort returns a copy of the listen address with the port replaced
// and the host forced to 127.0.0.1, honouring the --port CLI flag without
// ever opening the router to the network.
func (c *Config) SetListenPort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("config: invalid port %d", port)
	}
	c.Listen = net.JoinHostPort("127.0.0.1", fmt.Sprint(port))
	return nil
}

// ListenPort parses and returns the port from the listen address.
func (c *Config) ListenPort() (int, error) {
	_, portStr, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return 0, fmt.Errorf("config: invalid listen address %q: %w", c.Listen, err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		return 0, fmt.Errorf("config: invalid listen port %q: %w", portStr, err)
	}
	return port, nil
}
