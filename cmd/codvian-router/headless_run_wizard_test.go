package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

func TestParseArgsHeadlessWizard(t *testing.T) {
	opts, pos, err := parseArgs([]string{"serve", "--headless"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.headless || len(pos) != 1 || pos[0] != "serve" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}
	opts, _, err = parseArgs([]string{"serve", "--headless=false"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.headless {
		t.Fatal("headless = true, want false for --headless=false")
	}
	if _, _, err := parseArgs([]string{"serve", "--headless=maybe"}); err == nil {
		t.Fatal("expected error for --headless=maybe")
	}
	opts, pos, err = parseArgs([]string{"init", "--wizard"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.wizard || len(pos) != 1 || pos[0] != "init" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}
	// "--" ends flag parsing: run extra args pass through verbatim.
	opts, pos, err = parseArgs([]string{"run", "codex", "--model", "gpt-4o", "--", "--sandbox", "workspace-write"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.model != "gpt-4o" {
		t.Fatalf("model = %q, want gpt-4o", opts.model)
	}
	if len(pos) != 4 || pos[0] != "run" || pos[1] != "codex" || pos[2] != "--sandbox" || pos[3] != "workspace-write" {
		t.Fatalf("positional = %v, want [run codex --sandbox workspace-write]", pos)
	}
}

// requireGatewayKey refuses headless startup with no key: a missing
// config file, a config with zero keys, and an unreadable config
// all refuse — never a headless unauthenticated gateway.
func TestRequireGatewayKey(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope", "config.json")
	if err := requireGatewayKey(options{configPath: missing}); err == nil {
		t.Fatal("missing config: expected refusal, got nil")
	} else if !strings.Contains(err.Error(), "no gateway key") && !strings.Contains(err.Error(), "no config") {
		t.Fatalf("missing config: error %q does not name the missing key/config", err)
	}

	emptyPath := filepath.Join(dir, "empty.json")
	cfg := config.DefaultConfig(dir)
	if err := cfg.Save(emptyPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := requireGatewayKey(options{configPath: emptyPath}); err == nil {
		t.Fatal("zero keys: expected refusal, got nil")
	} else if !strings.Contains(err.Error(), "no gateway key") {
		t.Fatalf("zero keys: error %q does not name the missing key", err)
	}

	key, _, err := config.IssueKey("default", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatalf("AddGatewayKey: %v", err)
	}
	if err := cfg.Save(emptyPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := requireGatewayKey(options{configPath: emptyPath}); err != nil {
		t.Fatalf("one key: expected nil, got %v", err)
	}
}

func TestResolveRunCLIMissing(t *testing.T) {
	if _, err := resolveRunCLI("definitely-not-a-real-cli-xyz"); err == nil {
		t.Fatal("expected error for a missing CLI binary, got nil")
	} else if !strings.Contains(err.Error(), "definitely-not-a-real-cli-xyz") {
		t.Fatalf("error %q does not name the missing CLI", err)
	}
	if _, err := resolveRunCLI(""); err == nil {
		t.Fatal("expected error for an empty CLI name, got nil")
	}
	// A binary that exists resolves. "sh" is not on PATH on
	// Windows, so use the running test binary itself — it always
	// exists, on every platform.
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if _, err := resolveRunCLI(self); err != nil {
		t.Fatalf("resolveRunCLI(test binary): %v", err)
	}
}

func TestBuildRunEnv(t *testing.T) {
	env := buildRunEnv("http://127.0.0.1:20128/v1", "crk_testkey", "gpt-4o")
	want := map[string]string{
		"OPENAI_BASE_URL":         "http://127.0.0.1:20128/v1",
		"OPENAI_API_KEY":          "crk_testkey",
		"CODVIAN_ROUTER_BASE_URL": "http://127.0.0.1:20128/v1",
		"CODVIAN_ROUTER_API_KEY":  "crk_testkey",
		"CODVIAN_ROUTER_MODEL":    "gpt-4o",
		"CODEVN_API_ENDPOINT":     "http://127.0.0.1:20128/v1",
		"CODEVN_API_KEY":          "crk_testkey",
		"CODEVN_MODEL":            "gpt-4o",
		"ANTHROPIC_BASE_URL":      "http://127.0.0.1:20128/v1",
		"ANTHROPIC_API_KEY":       "crk_testkey",
	}
	if len(env) != len(want) {
		t.Fatalf("env = %v, want %d entries", env, len(want))
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		if want[k] != v {
			t.Fatalf("env %q = %q, want %q", k, v, want[k])
		}
	}
}

func TestGatewayPort(t *testing.T) {
	for listen, want := range map[string]int{
		"127.0.0.1:20128": 20128,
		"0.0.0.0:20199":   20199,
		"[::1]:20222":     20222,
	} {
		got, err := gatewayPort(listen)
		if err != nil || got != want {
			t.Fatalf("gatewayPort(%q) = %d, %v; want %d", listen, got, err, want)
		}
	}
	if _, err := gatewayPort("not-an-address"); err == nil {
		t.Fatal("expected error for a bad listen address")
	}
}

// run refuses before touching any gateway when the model flag or
// the CLI binary is missing — the two cheapest honest failures.
func TestRunRunMissingModelAndBinary(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := runRunIO(options{configPath: filepath.Join(t.TempDir(), "c.json")}, []string{"codex"}, &bytes.Buffer{}, &out, &errOut); code != 2 {
		t.Fatalf("no --model: exit = %d, want 2 (stderr %q)", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "--model") {
		t.Fatalf("no --model: stderr %q does not name --model", errOut.String())
	}
	errOut.Reset()
	if code := runRunIO(options{configPath: filepath.Join(t.TempDir(), "c.json"), model: "gpt-4o"}, []string{"definitely-not-a-real-cli-xyz"}, &bytes.Buffer{}, &out, &errOut); code != 1 {
		t.Fatalf("missing binary: exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "definitely-not-a-real-cli-xyz") {
		t.Fatalf("missing binary: stderr %q does not name the CLI", errOut.String())
	}
	if code := runRunIO(options{configPath: filepath.Join(t.TempDir(), "c.json"), model: "gpt-4o"}, nil, &bytes.Buffer{}, &out, &errOut); code != 2 {
		t.Fatalf("no CLI: exit = %d, want 2", code)
	}
}

// Wizard happy path, scripted stdin: empty port (keep default),
// provider skipped, so no network is touched at all — the config
// is created with one gateway key and the wizard reports done.
func TestWizardHappyPathSkipProvider(t *testing.T) {
	// Isolate HOME so DefaultDataDir lands in the temp dir.
	isolateUserDirs(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	in := strings.NewReader("\n\n") // keep port, skip provider
	var out bytes.Buffer
	stub := func(ctx context.Context, opts options, model string) (string, error) {
		t.Fatal("test call must not run with no provider")
		return "", nil
	}
	if code := runWizardIO(options{configPath: cfgPath}, in, &out, stub); code != 0 {
		t.Fatalf("wizard exit = %d, want 0 (out %q)", code, out.String())
	}
	if !strings.Contains(out.String(), "Wizard done.") {
		t.Fatalf("wizard output %q has no done line", out.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load written config: %v", err)
	}
	if len(cfg.GatewayKeys) != 1 {
		t.Fatalf("gateway keys = %d, want 1", len(cfg.GatewayKeys))
	}
	if len(cfg.Providers) != 0 {
		t.Fatalf("providers = %d, want 0 (skipped)", len(cfg.Providers))
	}
	// The once-only raw key is shown, never stored in the file.
	raw := out.String()
	if !strings.Contains(raw, "shown ONCE") {
		t.Fatalf("wizard output %q does not show the once-only key notice", raw)
	}
}

// Wizard skip path on an EXISTING config: nothing already
// configured is changed (same key count, same listen address),
// and EOF on stdin ends the prompts the same as empty answers.
func TestWizardSkipPathExistingConfig(t *testing.T) {
	isolateUserDirs(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	cfg := config.DefaultConfig(dir)
	key, _, err := config.IssueKey("default", []string{"chat", "models"}, 0)
	if err != nil {
		t.Fatalf("IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(key); err != nil {
		t.Fatalf("AddGatewayKey: %v", err)
	}
	if err := cfg.Save(cfgPath); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// EOF immediately: every prompt reads "".
	var out bytes.Buffer
	stub := func(ctx context.Context, opts options, model string) (string, error) {
		t.Fatal("test call must not run with no provider")
		return "", nil
	}
	if code := runWizardIO(options{configPath: cfgPath}, strings.NewReader(""), &out, stub); code != 0 {
		t.Fatalf("wizard exit = %d, want 0 (out %q)", code, out.String())
	}
	if !strings.Contains(out.String(), "config already exists") {
		t.Fatalf("wizard output %q does not say the config already exists", out.String())
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(after.GatewayKeys) != 1 || after.Listen != cfg.Listen {
		t.Fatalf("existing config changed: keys=%d listen=%q", len(after.GatewayKeys), after.Listen)
	}
}

// Wizard happy path WITH a provider: the provider/model/test steps
// run against a stubbed test call (no network), and the wizard
// reports the stubbed reply. The provider add itself is stubbed
// out by choosing a catalog id the test cannot add without a key —
// so this test drives the prompts only through the skip branch and
// asserts the summary; the full provider+test path is covered by
// the live smoke (real binary + mock upstream).
func TestWizardUnknownProviderIsHonestSkip(t *testing.T) {
	isolateUserDirs(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	// Unknown id, then empty on the retry prompt: the step is
	// skipped honestly instead of aborting the wizard.
	in := strings.NewReader("\nnot-a-provider\n\n")
	var out bytes.Buffer
	called := false
	stub := func(ctx context.Context, opts options, model string) (string, error) {
		called = true
		return "hello", nil
	}
	if code := runWizardIO(options{configPath: cfgPath}, in, &out, stub); code != 0 {
		t.Fatalf("wizard exit = %d, want 0 (out %q)", code, out.String())
	}
	if called {
		t.Fatal("test call ran with no provider added")
	}
	if !strings.Contains(out.String(), "unknown catalog provider") {
		t.Fatalf("wizard output %q does not report the unknown provider", out.String())
	}
	if !strings.Contains(out.String(), "Wizard done.") {
		t.Fatalf("wizard output %q has no done line", out.String())
	}
}
