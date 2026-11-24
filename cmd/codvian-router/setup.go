package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// runSetup handles `codvian-router setup [agent]`.
func runSetup(opts options, args []string) int {
	return runSetupIO(opts, args, os.Stdout, os.Stderr)
}

// AgentConfigManager defines the interface for configuring an agent CLI.
type AgentConfigManager interface {
	Name() string
	DefaultConfigPath() (string, error)
	ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error)
}

// CodeVNConfigManager manages configuration for CodeVN.
type CodeVNConfigManager struct{}

func (c *CodeVNConfigManager) Name() string { return "CodeVN" }

func (c *CodeVNConfigManager) DefaultConfigPath() (string, error) {
	if custom := os.Getenv("CODEVN_CONFIG_PATH"); custom != "" {
		return custom, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codevn", "config.json"), nil
}

func (c *CodeVNConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &cfgMap); err != nil {
			return nil, nil, fmt.Errorf("invalid existing CodeVN config JSON: %w", err)
		}
	}
	changes := make(map[string]any)

	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "auto/coding"
	}

	cfgMap["api_endpoint"] = endpoint
	changes["api_endpoint"] = endpoint

	cfgMap["model"] = model
	changes["model"] = model

	cfgMap["provider"] = "codvian"
	changes["provider"] = "codvian"

	cfgMap["wire_format"] = "openai"
	changes["wire_format"] = "openai"

	if apiKey != "" {
		cfgMap["api_key"] = apiKey
		changes["api_key"] = "[set]"
	} else {
		cfgMap["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
		changes["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
	}

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return out, changes, nil
}

// ClaudeConfigManager manages configuration for Claude Code.
type ClaudeConfigManager struct{}

func (c *ClaudeConfigManager) Name() string { return "Claude Code" }

func (c *ClaudeConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

func (c *ClaudeConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	cfgMap["anthropic_base_url"] = endpoint
	changes["anthropic_base_url"] = endpoint
	if model != "" {
		cfgMap["model"] = model
		changes["model"] = model
	}
	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// ContinueConfigManager manages configuration for Continue.
type ContinueConfigManager struct{}

func (c *ContinueConfigManager) Name() string { return "Continue" }

func (c *ContinueConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".continue", "config.json"), nil
}

func (c *ContinueConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "auto/coding"
	}
	modelsList, _ := cfgMap["models"].([]any)
	newModel := map[string]any{
		"title":    "Codvian Router - " + model,
		"provider": "openai",
		"model":    model,
		"apiBase":  endpoint,
	}
	if apiKey != "" {
		newModel["apiKey"] = apiKey
	} else {
		newModel["apiKey"] = "CODVIAN_ROUTER_API_KEY"
	}
	cfgMap["models"] = append([]any{newModel}, modelsList...)
	changes["models"] = fmt.Sprintf("prepended Codvian Router (%s)", model)
	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// --- P1.3 CLI Integration Expansion ---
// Ten additional AI coding client setup integrations.

// CodexConfigManager manages configuration for OpenAI Codex CLI.
type CodexConfigManager struct{}

func (c *CodexConfigManager) Name() string { return "codex" }

func (c *CodexConfigManager) DefaultConfigPath() (string, error) {
	// Codex CLI reads TOML config from ~/.codex/config.toml
	if custom := os.Getenv("CODEX_CONFIG_PATH"); custom != "" {
		return custom, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func (c *CodexConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	var out string
	if apiKey != "" {
		out = fmt.Sprintf(`# Codvian Router configuration
model = %q
base_url = %q
api_key = %q
`, model, endpoint, apiKey)
		changes["model"] = model
		changes["base_url"] = endpoint
		changes["api_key"] = "[set]"
	} else {
		out = fmt.Sprintf(`# Codvian Router configuration
model = %q
base_url = %q
api_key_env = "CODVIAN_ROUTER_API_KEY"
`, model, endpoint)
		changes["model"] = model
		changes["base_url"] = endpoint
		changes["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
	}

	return []byte(out), changes, nil
}

// OpenCodeConfigManager manages configuration for OpenCode.
type OpenCodeConfigManager struct{}

func (c *OpenCodeConfigManager) Name() string { return "opencode" }

func (c *OpenCodeConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json"), nil
}

func (c *OpenCodeConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	// OpenCode uses nested providers.openai.settings
	providers, _ := cfgMap["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
		cfgMap["providers"] = providers
	}
	openai, _ := providers["openai"].(map[string]any)
	if openai == nil {
		openai = make(map[string]any)
		providers["openai"] = openai
	}
	settings, _ := openai["settings"].(map[string]any)
	if settings == nil {
		settings = make(map[string]any)
		openai["settings"] = settings
	}
	if apiKey != "" {
		settings["apiKey"] = apiKey
		changes["providers.openai.settings.apiKey"] = "[set]"
	} else {
		settings["apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
		changes["providers.openai.settings.apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
	}
	settings["baseURL"] = endpoint
	changes["providers.openai.settings.baseURL"] = endpoint

	cfgMap["model"] = model
	changes["model"] = model

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// CursorConfigManager manages configuration for Cursor.
type CursorConfigManager struct{}

func (c *CursorConfigManager) Name() string { return "cursor" }

func (c *CursorConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

func (c *CursorConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	// Cursor uses MCP servers configuration under "mcpServers"
	mcpServers, _ := cfgMap["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = make(map[string]any)
		cfgMap["mcpServers"] = mcpServers
	}

	codvianMCP := map[string]any{
		"command": "codvian-router",
		"args":    []string{"mcp"},
		"env": map[string]any{
			"CODVIAN_ROUTER_BASE_URL": endpoint,
		},
	}
	if apiKey != "" {
		codvianMCP["env"].(map[string]any)["CODVIAN_ROUTER_API_KEY"] = apiKey
	}
	mcpServers["codvian-router"] = codvianMCP
	changes["mcpServers.codvian-router"] = "added Codvian Router MCP server"

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// ClineConfigManager manages configuration for Cline.
type ClineConfigManager struct{}

func (c *ClineConfigManager) Name() string { return "cline" }

func (c *ClineConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cline", "mcp.json"), nil
}

func (c *ClineConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	// Cline MCP settings use "mcpServers" key (clients.dev guide)
	mcpServers, _ := cfgMap["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = make(map[string]any)
		cfgMap["mcpServers"] = mcpServers
	}

	codvianMCP := map[string]any{
		"command": "codvian-router",
		"args":    []string{"mcp"},
		"env": map[string]any{
			"CODVIAN_ROUTER_BASE_URL": endpoint,
		},
	}
	if apiKey != "" {
		codvianMCP["env"].(map[string]any)["CODVIAN_ROUTER_API_KEY"] = apiKey
	}
	mcpServers["codvian-router"] = codvianMCP
	changes["mcpServers.codvian-router"] = "added Codvian Router MCP server"

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// AiderConfigManager manages configuration for Aider.
type AiderConfigManager struct{}

func (c *AiderConfigManager) Name() string { return "aider" }

func (c *AiderConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".aider.conf.yml"), nil
}

func (c *AiderConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	var out string
	if apiKey != "" {
		out = fmt.Sprintf(`# Codvian Router configuration for Aider
model = %q
openai-api-base = %q
openai-api-key = %q
`, model, endpoint, apiKey)
		changes["model"] = model
		changes["openai-api-base"] = endpoint
		changes["openai-api-key"] = "[set]"
	} else {
		out = fmt.Sprintf(`# Codvian Router configuration for Aider
model = %q
openai-api-base = %q
openai-api-key-env = CODVIAN_ROUTER_API_KEY
`, model, endpoint)
		changes["model"] = model
		changes["openai-api-base"] = endpoint
		changes["openai-api-key-env"] = "CODVIAN_ROUTER_API_KEY"
	}

	return []byte(out), changes, nil
}

// GeminiCLIConfigManager manages configuration for Gemini CLI.
type GeminiCLIConfigManager struct{}

func (c *GeminiCLIConfigManager) Name() string { return "gemini" }

func (c *GeminiCLIConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "settings.json"), nil
}

func (c *GeminiCLIConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gemini-2.5-pro"
	}

	cfgMap["endpoint"] = endpoint
	changes["endpoint"] = endpoint

	cfgMap["model"] = model
	changes["model"] = model

	if apiKey != "" {
		cfgMap["api_key"] = apiKey
		changes["api_key"] = "[set]"
	} else {
		cfgMap["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
		changes["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
	}

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// QwenCodeConfigManager manages configuration for Qwen Code.
type QwenCodeConfigManager struct{}

func (c *QwenCodeConfigManager) Name() string { return "qwen" }

func (c *QwenCodeConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".qwen", "settings.json"), nil
}

func (c *QwenCodeConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "qwen3-coder"
	}

	// Qwen Code uses nested model.name and model.baseUrl
	modelMap, _ := cfgMap["model"].(map[string]any)
	if modelMap == nil {
		modelMap = make(map[string]any)
		cfgMap["model"] = modelMap
	}
	modelMap["name"] = model
	changes["model.name"] = model

	modelMap["baseUrl"] = endpoint
	changes["model.baseUrl"] = endpoint

	if apiKey != "" {
		cfgMap["apiKey"] = apiKey
		changes["apiKey"] = "[set]"
	} else {
		cfgMap["apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
		changes["apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
	}

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// GooseConfigManager manages configuration for Goose.
type GooseConfigManager struct{}

func (c *GooseConfigManager) Name() string { return "goose" }

func (c *GooseConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "goose", "config.yaml"), nil
}

func (c *GooseConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	// Goose uses active_provider + providers map (YAML)
	out := fmt.Sprintf(`active_provider: codvian
providers:
  codvian:
    enabled: true
    model: %q
    configured: true
`, model)
	if apiKey != "" {
		out += fmt.Sprintf("    api_key: %q\n", apiKey)
		changes["api_key"] = "[set]"
	} else {
		out += `    api_key_env: CODVIAN_ROUTER_API_KEY
`
		changes["api_key_env"] = "CODVIAN_ROUTER_API_KEY"
	}
	changes["active_provider"] = "codvian"
	changes["providers.codvian"] = "added Codvian provider"

	return []byte(out), changes, nil
}

// RooCodeConfigManager manages configuration for Roo Code.
type RooCodeConfigManager struct{}

func (c *RooCodeConfigManager) Name() string { return "roo" }

func (c *RooCodeConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".roo", "mcp_settings.json"), nil
}

func (c *RooCodeConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}

	// Roo Code global MCP settings use "mcpServers" key in ~/.roo/mcp_settings.json
	mcpServers, _ := cfgMap["mcpServers"].(map[string]any)
	if mcpServers == nil {
		mcpServers = make(map[string]any)
		cfgMap["mcpServers"] = mcpServers
	}

	codvianMCP := map[string]any{
		"command": "codvian-router",
		"args":    []string{"mcp"},
		"env": map[string]any{
			"CODVIAN_ROUTER_BASE_URL": endpoint,
		},
	}
	if apiKey != "" {
		codvianMCP["env"].(map[string]any)["CODVIAN_ROUTER_API_KEY"] = apiKey
	}
	mcpServers["codvian-router"] = codvianMCP
	changes["mcpServers.codvian-router"] = "added Codvian Router MCP server"

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

// KiloCodeConfigManager manages configuration for Kilo Code.
type KiloCodeConfigManager struct{}

func (c *KiloCodeConfigManager) Name() string { return "kilo" }

func (c *KiloCodeConfigManager) DefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "kilo", "kilo.json"), nil
}

func (c *KiloCodeConfigManager) ApplyConfig(existing []byte, endpoint, model, apiKey string) ([]byte, map[string]any, error) {
	changes := make(map[string]any)
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}
	if model == "" {
		model = "gpt-5"
	}
	cfgMap := make(map[string]any)
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cfgMap)
	}

	// Kilo Code uses nested providers.openai.settings (OpenCode-compatible kilo.json)
	providers, _ := cfgMap["providers"].(map[string]any)
	if providers == nil {
		providers = make(map[string]any)
		cfgMap["providers"] = providers
	}
	openai, _ := providers["openai"].(map[string]any)
	if openai == nil {
		openai = make(map[string]any)
		providers["openai"] = openai
	}
	settings, _ := openai["settings"].(map[string]any)
	if settings == nil {
		settings = make(map[string]any)
		openai["settings"] = settings
	}
	if apiKey != "" {
		settings["apiKey"] = apiKey
		changes["providers.openai.settings.apiKey"] = "[set]"
	} else {
		settings["apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
		changes["providers.openai.settings.apiKeyEnv"] = "CODVIAN_ROUTER_API_KEY"
	}
	settings["baseURL"] = endpoint
	changes["providers.openai.settings.baseURL"] = endpoint

	cfgMap["model"] = model
	changes["model"] = model

	out, err := json.MarshalIndent(cfgMap, "", "  ")
	return out, changes, err
}

func getAgentManager(agentName string) (AgentConfigManager, error) {
	switch strings.ToLower(strings.TrimSpace(agentName)) {
	case "", "codevn":
		return &CodeVNConfigManager{}, nil
	case "claude", "claude-code":
		return &ClaudeConfigManager{}, nil
	case "continue":
		return &ContinueConfigManager{}, nil
	// P1.3 CLI Integration Expansion — ten additional clients
	case "codex", "codex-cli", "openai-codex":
		return &CodexConfigManager{}, nil
	case "opencode", "open-code":
		return &OpenCodeConfigManager{}, nil
	case "cursor":
		return &CursorConfigManager{}, nil
	case "cline":
		return &ClineConfigManager{}, nil
	case "aider":
		return &AiderConfigManager{}, nil
	case "gemini", "gemini-cli":
		return &GeminiCLIConfigManager{}, nil
	case "qwen", "qwen-code":
		return &QwenCodeConfigManager{}, nil
	case "goose":
		return &GooseConfigManager{}, nil
	case "roo", "roo-code":
		return &RooCodeConfigManager{}, nil
	case "kilo", "kilo-code":
		return &KiloCodeConfigManager{}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q: supported agents are codevn, claude, continue, codex, opencode, cursor, cline, aider, gemini, qwen, goose, roo, kilo", agentName)
	}
}

// runSetupIO executes the agent setup workflow with testable streams.
func runSetupIO(opts options, args []string, out, errOut io.Writer) int {
	agentName := "codevn"
	if len(args) > 0 {
		agentName = args[0]
	}

	mgr, err := getAgentManager(agentName)
	if err != nil {
		fmt.Fprintf(errOut, "codvian-router setup: %v\n", err)
		return 1
	}

	cfgTarget := opts.configPath
	if cfgTarget == "" {
		cfgTarget, err = mgr.DefaultConfigPath()
		if err != nil {
			fmt.Fprintf(errOut, "codvian-router setup: cannot resolve config path for %s: %v\n", mgr.Name(), err)
			return 1
		}
	}

	bakPath := cfgTarget + ".bak"

	// Revert flow
	if opts.revert {
		if _, err := os.Stat(bakPath); err != nil {
			fmt.Fprintf(errOut, "codvian-router setup revert: no backup found at %s\n", bakPath)
			return 1
		}
		data, err := os.ReadFile(bakPath)
		if err != nil {
			fmt.Fprintf(errOut, "codvian-router setup revert: cannot read backup %s: %v\n", bakPath, err)
			return 1
		}
		if err := os.WriteFile(cfgTarget, data, 0o600); err != nil {
			fmt.Fprintf(errOut, "codvian-router setup revert: cannot restore %s: %v\n", cfgTarget, err)
			return 1
		}
		fmt.Fprintf(out, "Restored %s configuration from backup %s\n", mgr.Name(), bakPath)
		return 0
	}

	// Resolve router endpoint URL
	endpoint := opts.endpoint
	if endpoint == "" {
		endpoint = opts.baseURL
	}
	if endpoint == "" {
		routerCfgPath, err := resolveConfigPath("")
		if err == nil {
			if rcfg, err := config.Load(routerCfgPath); err == nil && rcfg.Listen != "" {
				port, err := gatewayPort(rcfg.Listen)
				if err == nil {
					endpoint = fmt.Sprintf("http://127.0.0.1:%d/v1", port)
				}
			}
		}
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:20128/v1"
	}

	// Read existing config if present
	var existing []byte
	if data, err := os.ReadFile(cfgTarget); err == nil {
		existing = data
	}

	merged, changes, err := mgr.ApplyConfig(existing, endpoint, opts.model, opts.apiKey)
	if err != nil {
		fmt.Fprintf(errOut, "codvian-router setup: cannot generate configuration: %v\n", err)
		return 1
	}

	if opts.dryRun {
		fmt.Fprintf(out, "=== DRY RUN: %s configuration (%s) ===\n", mgr.Name(), cfgTarget)
		fmt.Fprintf(out, "Proposed changes:\n")
		for k, v := range changes {
			fmt.Fprintf(out, "  + %s: %v\n", k, v)
		}
		fmt.Fprintf(out, "\nProposed config contents:\n%s\n", string(merged))
		return 0
	}

	// Take backup before overwriting
	if len(existing) > 0 {
		_ = os.WriteFile(bakPath, existing, 0o600)
		timestampBak := fmt.Sprintf("%s.%d", bakPath, time.Now().Unix())
		_ = os.WriteFile(timestampBak, existing, 0o600)
	}

	if err := os.MkdirAll(filepath.Dir(cfgTarget), 0o700); err != nil {
		fmt.Fprintf(errOut, "codvian-router setup: cannot create config directory: %v\n", err)
		return 1
	}

	if err := os.WriteFile(cfgTarget, merged, 0o600); err != nil {
		fmt.Fprintf(errOut, "codvian-router setup: cannot write configuration to %s: %v\n", cfgTarget, err)
		return 1
	}

	fmt.Fprintf(out, "Successfully configured %s to use Codvian Router!\n", mgr.Name())
	fmt.Fprintf(out, "Config file: %s\n", cfgTarget)
	if len(existing) > 0 {
		fmt.Fprintf(out, "Backup created: %s\n", bakPath)
	}
	fmt.Fprintf(out, "Applied settings:\n")
	for k, v := range changes {
		fmt.Fprintf(out, "  - %s: %v\n", k, v)
	}
	fmt.Fprintf(out, "\nTo launch %s:\n", mgr.Name())
	if agentName == "codevn" {
		fmt.Fprintf(out, "  codevn\n")
		fmt.Fprintf(out, "or run through router:\n")
		fmt.Fprintf(out, "  codvian-router run codevn --model %s\n", opts.model)
	} else if agentName == "codex" || agentName == "codex-cli" || agentName == "openai-codex" {
		fmt.Fprintf(out, "  codex\n")
		fmt.Fprintf(out, "or run through router:\n")
		fmt.Fprintf(out, "  codvian-router run codex --model %s\n", opts.model)
	} else {
		fmt.Fprintf(out, "  Run %s and it will read the configured Codvian Router endpoint.\n", mgr.Name())
	}
	return 0
}
