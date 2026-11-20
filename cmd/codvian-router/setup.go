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

func getAgentManager(agentName string) (AgentConfigManager, error) {
	switch strings.ToLower(strings.TrimSpace(agentName)) {
	case "", "codevn":
		return &CodeVNConfigManager{}, nil
	case "claude", "claude-code":
		return &ClaudeConfigManager{}, nil
	case "continue":
		return &ContinueConfigManager{}, nil
	default:
		return nil, fmt.Errorf("unknown agent %q: supported agents are codevn, claude, continue", agentName)
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
	}
	return 0
}
