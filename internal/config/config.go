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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// CurrentVersion is the only config version this build understands.
const CurrentVersion = 1

// DefaultListen is the default bind address. Localhost only, never 0.0.0.0.
const DefaultListen = "127.0.0.1:20128"

// DefaultPort is the port portion of DefaultListen.
const DefaultPort = 20128

// Config is the on-disk router configuration (version 1).
type Config struct {
	Version          int              `json:"version"`
	Listen           string           `json:"listen"`
	DataDir          string           `json:"data_dir"`
	GatewayKeys      []GatewayKey     `json:"gateway_keys"`
	Providers        []ProviderConfig `json:"providers"`
	BudgetUSD        float64          `json:"budget_usd"`
	FailClosed       bool             `json:"fail_closed"`
	TelemetryEnabled bool             `json:"telemetry_enabled"`
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
		if p.BaseURL == "" {
			return fmt.Errorf("config: provider %q has an empty base_url", p.ID)
		}
	}
	return nil
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
