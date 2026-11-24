package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupCodeVNDryRun(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	var out, errOut bytes.Buffer
	opts := options{
		configPath: cfgPath,
		dryRun:     true,
		model:      "auto/coding",
		endpoint:   "http://127.0.0.1:20128/v1",
	}

	exitCode := runSetupIO(opts, []string{"codevn"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}

	outStr := out.String()
	if !strings.Contains(outStr, "DRY RUN") {
		t.Errorf("expected output to mention DRY RUN, got: %s", outStr)
	}
	if !strings.Contains(outStr, "http://127.0.0.1:20128/v1") {
		t.Errorf("expected output to contain endpoint, got: %s", outStr)
	}
	if !strings.Contains(outStr, "auto/coding") {
		t.Errorf("expected output to contain model, got: %s", outStr)
	}

	// Verify file was NOT created
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Fatalf("dry run must not create file on disk")
	}
}

func TestSetupCodeVNFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sub", "config.json")

	var out, errOut bytes.Buffer
	opts := options{
		configPath: cfgPath,
		model:      "auto/coding",
		endpoint:   "http://127.0.0.1:20128/v1",
		apiKey:     "crk_test_key_123",
	}

	exitCode := runSetupIO(opts, []string{"codevn"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading created config: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	if parsed["api_endpoint"] != "http://127.0.0.1:20128/v1" {
		t.Errorf("api_endpoint = %v, want http://127.0.0.1:20128/v1", parsed["api_endpoint"])
	}
	if parsed["model"] != "auto/coding" {
		t.Errorf("model = %v, want auto/coding", parsed["model"])
	}
	if parsed["provider"] != "codvian" {
		t.Errorf("provider = %v, want codvian", parsed["provider"])
	}
	if parsed["wire_format"] != "openai" {
		t.Errorf("wire_format = %v, want openai", parsed["wire_format"])
	}
	if parsed["api_key"] != "crk_test_key_123" {
		t.Errorf("api_key = %v, want crk_test_key_123", parsed["api_key"])
	}
}

func TestSetupCodeVNMergePreservesUserSettings(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")

	initial := map[string]any{
		"theme":        "monokai",
		"custom_tools": []any{"git", "pytest"},
		"max_tokens":   8192,
	}
	initialBytes, _ := json.Marshal(initial)
	if err := os.WriteFile(cfgPath, initialBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	opts := options{
		configPath: cfgPath,
		model:      "auto/best-coding",
		endpoint:   "http://127.0.0.1:20128/v1",
	}

	exitCode := runSetupIO(opts, []string{"codevn"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}

	// Verify backup exists
	bakPath := cfgPath + ".bak"
	bakData, err := os.ReadFile(bakPath)
	if err != nil {
		t.Fatalf("expected backup at %s: %v", bakPath, err)
	}
	if !strings.Contains(string(bakData), "monokai") {
		t.Errorf("backup does not contain original content")
	}

	// Read merged file
	mergedData, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading merged config: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(mergedData, &merged); err != nil {
		t.Fatalf("unmarshal merged: %v", err)
	}

	// Check preserved fields
	if merged["theme"] != "monokai" {
		t.Errorf("theme = %v, want monokai (preserved)", merged["theme"])
	}
	if merged["max_tokens"] != float64(8192) {
		t.Errorf("max_tokens = %v, want 8192 (preserved)", merged["max_tokens"])
	}

	// Check added fields
	if merged["api_endpoint"] != "http://127.0.0.1:20128/v1" {
		t.Errorf("api_endpoint = %v, want http://127.0.0.1:20128/v1", merged["api_endpoint"])
	}
	if merged["model"] != "auto/best-coding" {
		t.Errorf("model = %v, want auto/best-coding", merged["model"])
	}
}

func TestSetupCodeVNRevert(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	bakPath := cfgPath + ".bak"

	originalContent := `{"theme":"solarized"}`
	mutatedContent := `{"theme":"solarized","api_endpoint":"http://127.0.0.1:20128/v1"}`

	if err := os.WriteFile(cfgPath, []byte(mutatedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bakPath, []byte(originalContent), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	opts := options{
		configPath: cfgPath,
		revert:     true,
	}

	exitCode := runSetupIO(opts, []string{"codevn"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}

	restored, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != originalContent {
		t.Errorf("restored = %q, want original %q", string(restored), originalContent)
	}
}

func TestSetupClaudeAndContinue(t *testing.T) {
	dir := t.TempDir()

	// Claude
	claudePath := filepath.Join(dir, "claude.json")
	optsClaude := options{
		configPath: claudePath,
		endpoint:   "http://127.0.0.1:20128/v1",
	}
	var out, errOut bytes.Buffer
	if exitCode := runSetupIO(optsClaude, []string{"claude"}, &out, &errOut); exitCode != 0 {
		t.Fatalf("claude setup failed: %s", errOut.String())
	}
	data, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "anthropic_base_url") {
		t.Errorf("expected anthropic_base_url in claude config: %s", string(data))
	}

	// Continue
	continuePath := filepath.Join(dir, "continue.json")
	optsContinue := options{
		configPath: continuePath,
		model:      "auto/coding",
		endpoint:   "http://127.0.0.1:20128/v1",
	}
	out.Reset()
	errOut.Reset()
	if exitCode := runSetupIO(optsContinue, []string{"continue"}, &out, &errOut); exitCode != 0 {
		t.Fatalf("continue setup failed: %s", errOut.String())
	}
	data, err = os.ReadFile(continuePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Codvian Router") {
		t.Errorf("expected Codvian Router model in continue config: %s", string(data))
	}
}

// --- P1.3 tests for the ten new CLI integration managers ---

func TestSetupCodexDryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := options{configPath: "/tmp/codex_test.toml", dryRun: true, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1", apiKey: "crk_test_key_123"}
	exitCode := runSetupIO(opts, []string{"codex"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("expected DRY RUN, got: %s", out.String())
	}
}

func TestSetupOpenCodeDryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := options{configPath: "/tmp/opencode_test.json", dryRun: true, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"opencode"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("expected DRY RUN, got: %s", out.String())
	}
}

func TestSetupCursorDryRun(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := options{configPath: "/tmp/cursor_test.json", dryRun: true, endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"cursor"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	if !strings.Contains(out.String(), "DRY RUN") {
		t.Errorf("expected DRY RUN, got: %s", out.String())
	}
}

func TestSetupUnknownAgentRejected(t *testing.T) {
	var out, errOut bytes.Buffer
	opts := options{}
	exitCode := runSetupIO(opts, []string{"nonexistent-agent"}, &out, &errOut)
	if exitCode == 0 {
		t.Fatalf("expected non-zero exit for unknown agent")
	}
	if !strings.Contains(errOut.String(), "unknown agent") {
		t.Errorf("expected 'unknown agent' error, got: %s", errOut.String())
	}
}

func TestSetupCodexFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1", apiKey: "crk_test_key_123"}
	exitCode := runSetupIO(opts, []string{"codex"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "gpt-5") || !strings.Contains(content, "http://127.0.0.1:20128/v1") || !strings.Contains(content, "crk_test_key_123") {
		t.Errorf("codex config missing expected fields: %s", content)
	}
}

func TestSetupClineFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1", apiKey: "crk_test_key_123"}
	exitCode := runSetupIO(opts, []string{"cline"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["api_base_url"] != "http://127.0.0.1:20128/v1" || parsed["model"] != "gpt-5" || parsed["api_key"] != "crk_test_key_123" {
		t.Errorf("cline config wrong: %v", parsed)
	}
}

func TestSetupAiderFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".aider.conf.yml")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1", apiKey: "crk_test_key_123"}
	exitCode := runSetupIO(opts, []string{"aider"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "gpt-5") || !strings.Contains(content, "http://127.0.0.1:20128/v1") || !strings.Contains(content, "crk_test_key_123") {
		t.Errorf("aider config missing fields: %s", content)
	}
}

func TestSetupGeminiFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gemini-2.5-pro", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"gemini"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["endpoint"] != "http://127.0.0.1:20128/v1" || parsed["model"] != "gemini-2.5-pro" {
		t.Errorf("gemini config wrong: %v", parsed)
	}
}

func TestSetupQwenFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "qwen3-coder", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"qwen"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["base_url"] != "http://127.0.0.1:20128/v1" || parsed["model"] != "qwen3-coder" {
		t.Errorf("qwen config wrong: %v", parsed)
	}
}

func TestSetupGooseFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"goose"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "gpt-5") || !strings.Contains(content, "http://127.0.0.1:20128/v1") {
		t.Errorf("goose config missing fields: %s", content)
	}
}

func TestSetupRooFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "settings.json")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"roo"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["provider"] != "openai" || parsed["base_url"] != "http://127.0.0.1:20128/v1" {
		t.Errorf("roo config wrong: %v", parsed)
	}
}

func TestSetupKiloFreshWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	var out, errOut bytes.Buffer
	opts := options{configPath: cfgPath, model: "gpt-5", endpoint: "http://127.0.0.1:20128/v1"}
	exitCode := runSetupIO(opts, []string{"kilo"}, &out, &errOut)
	if exitCode != 0 {
		t.Fatalf("expected exit 0, got %d: %s", exitCode, errOut.String())
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["base_url"] != "http://127.0.0.1:20128/v1" || parsed["model"] != "gpt-5" {
		t.Errorf("kilo config wrong: %v", parsed)
	}
}
