package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/feedback"
	"github.com/raunakiitp/codvian-router/internal/ledger"
	"github.com/raunakiitp/codvian-router/internal/router"
)

func TestParseArgsLowEnd(t *testing.T) {
	opts, pos, err := parseArgs([]string{"--low-end", "serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if !opts.lowEnd || len(pos) != 1 || pos[0] != "serve" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}
	opts, _, err = parseArgs([]string{"serve", "--low-end=false"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.lowEnd {
		t.Fatalf("lowEnd = true, want false for --low-end=false")
	}
	if _, _, err := parseArgs([]string{"--low-end=maybe"}); err == nil {
		t.Fatal("expected error for --low-end=maybe")
	}
	// Default: low-end off.
	opts, _, err = parseArgs([]string{"serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.lowEnd {
		t.Fatalf("lowEnd = true, want false by default")
	}
}

func TestParseArgsSubcommandsAndFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"--config", "/tmp/c.json", "--port=20199", "serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.configPath != "/tmp/c.json" || opts.port != 20199 {
		t.Fatalf("opts = %+v", opts)
	}
	if len(pos) != 1 || pos[0] != "serve" {
		t.Fatalf("positional = %v", pos)
	}

	opts, pos, err = parseArgs([]string{"eval", "--model", "bench-model"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.model != "bench-model" || len(pos) != 1 || pos[0] != "eval" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}

	opts, _, err = parseArgs([]string{"--listen=0.0.0.0:20128", "serve"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.listen != "0.0.0.0:20128" {
		t.Fatalf("listen = %q", opts.listen)
	}

	// No subcommand defaults to serve.
	_, pos, err = parseArgs(nil)
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if len(pos) != 0 {
		t.Fatalf("positional = %v, want empty", pos)
	}

	if _, _, err := parseArgs([]string{"--nope"}); err == nil {
		t.Fatal("expected error for unknown flag")
	}
	if _, _, err := parseArgs([]string{"--port", "abc"}); err == nil {
		t.Fatal("expected error for non-numeric --port")
	}
	if _, _, err := parseArgs([]string{"--listen"}); err == nil {
		t.Fatal("expected error for --listen without a value")
	}
}

func TestRunHelpAndUnknown(t *testing.T) {
	if code := run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d, want 0", code)
	}
	if code := run([]string{"bogus-cmd"}); code != 2 {
		t.Fatalf("unknown subcommand exit = %d, want 2", code)
	}
	if code := run([]string{"eval"}); code != 2 {
		t.Fatalf("eval without --model exit = %d, want 2", code)
	}
}

// TestRunVersion (CHECKLIST 321): the install happy path starts with
// `codvian-router --version`. Before this slice neither --version nor
// the version subcommand existed — both exited 2 as unknown flag /
// unknown subcommand. The version printed must match the Version
// constant the startup lines use, so an installed binary reports the
// same version it serves with.
func TestRunVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}} {
		if code := run(args); code != 0 {
			t.Fatalf("run(%v) exit = %d, want 0", args, code)
		}
	}
	// parseArgs maps --version to the version positional, like --help.
	if _, pos, err := parseArgs([]string{"--version"}); err != nil || len(pos) != 1 || pos[0] != "version" {
		t.Fatalf("parseArgs(--version) = %v, %v, want [version], nil", pos, err)
	}
}

func TestRunProviderAddCatalog(t *testing.T) {
	// Isolated HOME: init writes its config + data dir under it.
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()

	// Catalog add with the Fireworks alt-header variant, base URL
	// overridden to the mock.
	if code := run([]string{"provider", "add-catalog", "fireworks", "fw-test-key",
		"--endpoint", "chat", "--variant", "alt-header", "--base-url", mock.URL}); code != 0 {
		t.Fatalf("provider add-catalog exit = %d, want 0", code)
	}
	if code := run([]string{"provider", "list"}); code != 0 {
		t.Fatalf("provider list exit = %d, want 0", code)
	}

	// Exchange auth is refused at the CLI too (Vertex needs a
	// service-account exchange, never a static header).
	if code := run([]string{"provider", "add-catalog", "vertex", "sa-json"}); code == 0 {
		t.Fatalf("provider add-catalog vertex exit = %d, want non-zero", code)
	}

	// Unknown action / missing args are usage errors.
	if code := run([]string{"provider"}); code != 2 {
		t.Fatalf("provider without action exit = %d, want 2", code)
	}
	if code := run([]string{"provider", "add-catalog", "groq"}); code != 2 {
		t.Fatalf("provider add-catalog without key exit = %d, want 2", code)
	}
}

func TestEffectiveListen(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:20128"}

	got, err := effectiveListen(cfg, options{})
	if err != nil || got != "127.0.0.1:20128" {
		t.Fatalf("default: got %q, err %v", got, err)
	}

	got, err = effectiveListen(cfg, options{listen: "127.0.0.1:9999"})
	if err != nil || got != "127.0.0.1:9999" {
		t.Fatalf("loopback override: got %q, err %v", got, err)
	}

	got, err = effectiveListen(cfg, options{listen: "0.0.0.0:20128"})
	if err != nil || got != "0.0.0.0:20128" {
		t.Fatalf("0.0.0.0 override: got %q, err %v", got, err)
	}

	for _, bad := range []string{
		"192.168.1.5:20128", // arbitrary LAN address: not allowed
		"example.com:20128", // hostname: not allowed
		"127.0.0.1",         // missing port
		"127.0.0.1:0",       // port out of range
		"127.0.0.1:70000",   // port out of range
		"127.0.0.1:abc",     // non-numeric port
	} {
		if _, err := effectiveListen(cfg, options{listen: bad}); err == nil {
			t.Fatalf("--listen %q: expected error, got nil", bad)
		} else if !strings.Contains(err.Error(), "--listen") {
			t.Fatalf("--listen %q: error %v does not name --listen", bad, err)
		}
	}
}

func TestParseArgsKeyFile(t *testing.T) {
	opts, pos, err := parseArgs([]string{"init", "--key-file", "/tmp/k.txt"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.keyFile != "/tmp/k.txt" || len(pos) != 1 || pos[0] != "init" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}
	opts, _, err = parseArgs([]string{"--key-file=/tmp/k2.txt", "init"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.keyFile != "/tmp/k2.txt" {
		t.Fatalf("keyFile = %q, want /tmp/k2.txt", opts.keyFile)
	}
	if _, _, err := parseArgs([]string{"init", "--key-file"}); err == nil {
		t.Fatal("expected error for --key-file without a value")
	}
}

// captureStdout runs f with os.Stdout redirected to a pipe and
// returns everything f printed.
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	f()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

func TestRunInitKeyFile(t *testing.T) {
	home := isolateUserDirs(t)
	keyPath := filepath.Join(home, "gateway.key")
	var code int
	out := captureStdout(t, func() {
		code = run([]string{"init", "--key-file", keyPath})
	})
	if code != 0 {
		t.Fatalf("init --key-file exit = %d, want 0 (stdout %q)", code, out)
	}
	// The key file is 0600 and holds a key that authenticates
	// against the saved config. Windows has no Unix permission
	// bits (files report 0666), so the exact mode is only
	// asserted off Windows.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("Stat key file: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("key file perms = %o, want 0600", info.Mode().Perm())
	}
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("ReadFile key: %v", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		t.Fatal("key file is empty")
	}
	cfgPath, err := config.DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load config: %v", err)
	}
	if _, ok := cfg.VerifyGatewayKey(key); !ok {
		t.Error("key from --key-file does not verify against the saved config")
	}
	// The once-only key never reaches stdout: a scripted install
	// must not leak it into logs.
	if strings.Contains(out, key) {
		t.Errorf("stdout contains the raw key — --key-file must keep it out of stdout:\n%s", out)
	}
	if !strings.Contains(out, keyPath) {
		t.Errorf("stdout does not name the key file:\n%s", out)
	}
	// No leftover temp files next to the key.
	if temps, _ := filepath.Glob(filepath.Join(home, ".codvian-key-*.tmp")); len(temps) != 0 {
		t.Errorf("leftover key temp files: %v", temps)
	}
	// A second init with the same key file path refuses: the
	// existing key file is never clobbered.
	home2 := isolateUserDirs(t)
	existing := filepath.Join(home2, "gateway.key")
	if err := os.WriteFile(existing, []byte("already-saved\n"), 0o600); err != nil {
		t.Fatalf("WriteFile existing: %v", err)
	}
	if code := run([]string{"init", "--key-file", existing}); code == 0 {
		t.Error("init with an existing --key-file exit = 0, want non-zero (refuse to overwrite)")
	}
	if got, _ := os.ReadFile(existing); string(got) != "already-saved\n" {
		t.Errorf("existing key file was modified: %q", got)
	}
	// Resolve the config path the same way the CLI does (on
	// Windows it is under %AppData%, not $HOME/.config): no
	// config may have been created by the refused init.
	cfgPath2, err := config.DefaultConfigPath()
	if err != nil {
		t.Fatalf("DefaultConfigPath: %v", err)
	}
	if _, err := os.Stat(cfgPath2); !os.IsNotExist(err) {
		t.Error("config was created despite the refused key file")
	}
}

func TestRunDBImport(t *testing.T) {
	// Isolated HOME: init writes its config + data dir under it.
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	// Resolve the data dir the same way the CLI does.
	path, err := resolveConfigPath("")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// An export file with two events (hand-built in the exact
	// export shape — the server route writes the same shape).
	export := `{"format":"codvian-router-db-export/v1","exported_at":"2026-11-18T00:00:00Z","config":{"version":1},"usage":{"events":[{"ts":"2026-11-18T09:00:00Z","gateway_key_id":"k1","provider":"openai","model":"gpt-4o-mini","prompt_tokens":20,"completion_tokens":8,"total_tokens":28,"split_known":true,"cost_usd":0,"ok":true,"latency_ms":42},{"ts":"2026-11-18T09:01:00Z","gateway_key_id":"k1","provider":"anthropic","model":"claude-sonnet-4-5","prompt_tokens":30,"completion_tokens":12,"total_tokens":42,"split_known":true,"cost_usd":0,"ok":false,"latency_ms":55}],"events_total":2,"events_included":2,"truncated":false},"secrets":"redacted","note":"test"}`
	file := filepath.Join(t.TempDir(), "export.json")
	if err := os.WriteFile(file, []byte(export), 0o600); err != nil {
		t.Fatal(err)
	}
	// Wrong phrase: refused, nothing imported.
	if code := run([]string{"db", "import", file, "--confirm", "EXPORT"}); code == 0 {
		t.Fatalf("db import wrong phrase exit = %d, want non-zero", code)
	}
	// Missing file: refused.
	if code := run([]string{"db", "import", filepath.Join(t.TempDir(), "nope.json"), "--confirm", "IMPORT"}); code == 0 {
		t.Fatalf("db import missing file exit = %d, want non-zero", code)
	}
	// Happy path: 2 events merged.
	if code := run([]string{"db", "import", file, "--confirm", "IMPORT"}); code != 0 {
		t.Fatalf("db import exit = %d, want 0", code)
	}
	led, err := ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := led.Summary(context.Background(), time.Time{}, time.Time{})
	_ = led.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.TotalTokens != 70 {
		t.Fatalf("ledger after db import = %+v, want 2 requests / 70 tokens", sum)
	}
	// Re-import the same file: exit 0, still 2 requests — the
	// duplicates are skipped, never double-counted.
	if code := run([]string{"db", "import", file, "--confirm", "IMPORT"}); code != 0 {
		t.Fatalf("db re-import exit = %d, want 0", code)
	}
	led, err = ledger.Open(filepath.Join(cfg.DataDir, "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	sum, err = led.Summary(context.Background(), time.Time{}, time.Time{})
	_ = led.Close()
	if err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 2 || sum.TotalTokens != 70 {
		t.Fatalf("ledger after db re-import = %+v, want still 2 requests / 70 tokens", sum)
	}
	// Corrupt file (wrong format version): refused, ledger untouched.
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"format":"codvian-router-db-export/v2","usage":{"events":[],"events_total":0}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"db", "import", bad, "--confirm", "IMPORT"}); code == 0 {
		t.Fatalf("db import wrong version exit = %d, want non-zero", code)
	}
	// Unknown action / missing args are usage errors.
	if code := run([]string{"db"}); code != 2 {
		t.Fatalf("db without action exit = %d, want 2", code)
	}
	if code := run([]string{"db", "wipe"}); code != 2 {
		t.Fatalf("db unknown action exit = %d, want 2", code)
	}
	if code := run([]string{"db", "import"}); code != 2 {
		t.Fatalf("db import without file exit = %d, want 2", code)
	}
}

// TestParseArgsCatalogAuditFlags pins the CHECKLIST 271-272 fix:
// every `catalog audit` flag parses after the subcommand, in both
// "--flag value" and "--flag=value" form, and lands on options.
// Before the fix the global parser rejected these as "unknown flag"
// and the audit handler never ran.
func TestParseArgsCatalogAuditFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"catalog", "audit",
		"--out-dir", "/tmp/out", "--web-json", "/tmp/web.json",
		"--models-dev-url", "https://example.invalid/api.json",
		"--skip-reachability", "--concurrency", "4",
		"--probe-timeout", "5", "--triage-summary", "/tmp/triage.json"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if len(pos) != 2 || pos[0] != "catalog" || pos[1] != "audit" {
		t.Fatalf("positional = %v", pos)
	}
	if opts.catalogOutDir != "/tmp/out" || opts.catalogWebJSON != "/tmp/web.json" ||
		opts.catalogModelsDevURL != "https://example.invalid/api.json" ||
		!opts.catalogSkipReachability || opts.catalogConcurrency != 4 ||
		opts.catalogProbeTimeoutSec != 5 || opts.catalogTriageSummary != "/tmp/triage.json" {
		t.Fatalf("opts = %+v", opts)
	}
	if !opts.catalogWebJSONSet || !opts.catalogTriageSummarySet {
		t.Fatalf("set bits: webJSON %v triage %v", opts.catalogWebJSONSet, opts.catalogTriageSummarySet)
	}

	// = form, before the subcommand; an empty --triage-summary=
	// disables embedding (and --skip-reachability=false unsets).
	opts, _, err = parseArgs([]string{"--out-dir=/tmp/o2", "--web-json=/tmp/w2.json",
		"--models-dev-url=https://example.invalid/b.json", "--skip-reachability=false",
		"--concurrency=8", "--probe-timeout=30", "--triage-summary=", "catalog", "audit"})
	if err != nil {
		t.Fatalf("parseArgs (= form): %v", err)
	}
	if opts.catalogOutDir != "/tmp/o2" || opts.catalogWebJSON != "/tmp/w2.json" ||
		opts.catalogModelsDevURL != "https://example.invalid/b.json" ||
		opts.catalogSkipReachability || opts.catalogConcurrency != 8 ||
		opts.catalogProbeTimeoutSec != 30 || opts.catalogTriageSummary != "" {
		t.Fatalf("opts (= form) = %+v", opts)
	}
	if !opts.catalogWebJSONSet || !opts.catalogTriageSummarySet {
		t.Fatalf("set bits (= form): webJSON %v triage %v", opts.catalogWebJSONSet, opts.catalogTriageSummarySet)
	}

	// Defaults: nothing set.
	opts, _, err = parseArgs([]string{"catalog", "audit"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.catalogOutDir != "" || opts.catalogWebJSON != "" || opts.catalogModelsDevURL != "" ||
		opts.catalogSkipReachability || opts.catalogConcurrency != 0 ||
		opts.catalogProbeTimeoutSec != 0 || opts.catalogTriageSummary != "" ||
		opts.catalogWebJSONSet || opts.catalogTriageSummarySet {
		t.Fatalf("opts (defaults) = %+v", opts)
	}

	// Validation errors — the same bounds the old handler enforced.
	for _, bad := range [][]string{
		{"catalog", "audit", "--out-dir"},
		{"catalog", "audit", "--out-dir="},
		{"catalog", "audit", "--models-dev-url="},
		{"catalog", "audit", "--web-json"},
		{"catalog", "audit", "--triage-summary"},
		{"catalog", "audit", "--concurrency", "0"},
		{"catalog", "audit", "--concurrency=abc"},
		{"catalog", "audit", "--probe-timeout", "0"},
		{"catalog", "audit", "--probe-timeout=-3"},
		{"catalog", "audit", "--skip-reachability=maybe"},
	} {
		if _, _, err := parseArgs(bad); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

// TestParseArgsEvalFeedbackFlags pins the CHECKLIST 271-272 fix
// for the other previously-broken command: `eval-feedback
// --feedback-db` and `--cutoff` parse after the subcommand.
func TestParseArgsEvalFeedbackFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"eval-feedback",
		"--feedback-db", "/tmp/fb.db", "--cutoff", "2026-11-18T00:00:00Z"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if len(pos) != 1 || pos[0] != "eval-feedback" {
		t.Fatalf("positional = %v", pos)
	}
	if opts.evalFeedbackDB != "/tmp/fb.db" {
		t.Fatalf("evalFeedbackDB = %q", opts.evalFeedbackDB)
	}
	wantCutoff := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	if !opts.evalCutoff.Equal(wantCutoff) {
		t.Fatalf("evalCutoff = %v, want %v", opts.evalCutoff, wantCutoff)
	}

	// = form, before the subcommand.
	opts, _, err = parseArgs([]string{"--feedback-db=/tmp/f2.db", "--cutoff=2026-01-02T03:04:05Z", "eval-feedback"})
	if err != nil {
		t.Fatalf("parseArgs (= form): %v", err)
	}
	if opts.evalFeedbackDB != "/tmp/f2.db" {
		t.Fatalf("evalFeedbackDB (= form) = %q", opts.evalFeedbackDB)
	}
	if !opts.evalCutoff.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("evalCutoff (= form) = %v", opts.evalCutoff)
	}

	// Defaults: no store override, zero cutoff (default 70/30 split).
	opts, _, err = parseArgs([]string{"eval-feedback"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.evalFeedbackDB != "" || !opts.evalCutoff.IsZero() {
		t.Fatalf("opts (defaults) = %+v", opts)
	}

	for _, bad := range [][]string{
		{"eval-feedback", "--feedback-db"},
		{"eval-feedback", "--cutoff"},
		{"eval-feedback", "--cutoff", "not-a-time"},
		{"eval-feedback", "--cutoff=2026-11-18"},
	} {
		if _, _, err := parseArgs(bad); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

// TestParseArgsDocumentedFlagsSweep pins the parser against the
// usage text: every --flag the help documents must parse, in both
// "--flag value" and "--flag=value" form, after the subcommand.
// A flag that is documented but unregistered is the exact bug
// this slice fixes, so the table is the regression net for it.
func TestParseArgsDocumentedFlagsSweep(t *testing.T) {
	valueFlags := []struct{ flag, value string }{
		{"--config", "/tmp/c.json"},
		{"--port", "20199"},
		{"--listen", "127.0.0.1:20199"},
		{"--model", "bench-model"},
		{"--key-file", "/tmp/k.txt"},
		{"--endpoint", "chat"},
		{"--variant", "alt-header"},
		{"--base-url", "http://127.0.0.1:9"},
		{"--name", "test-name"},
		{"--provider", "openai"},
		{"--confirm", "IMPORT"},
		{"--api-key", "test-key-123"},
		{"--api-key-env", "TEST_KEY_ENV"},
		{"--dir", "/tmp/rel"},
		{"--repo", "owner/repo"},
		{"--key", "/tmp/pub.key"},
		{"--image", "ghcr.io/x/y:tag"},
		{"--download-base-url", "https://example.invalid"},
		{"--out-dir", "/tmp/out"},
		{"--web-json", "/tmp/web.json"},
		{"--models-dev-url", "https://example.invalid/api.json"},
		{"--concurrency", "4"},
		{"--probe-timeout", "5"},
		{"--triage-summary", "/tmp/triage.json"},
		{"--feedback-db", "/tmp/fb.db"},
		{"--cutoff", "2026-11-18T00:00:00Z"},
	}
	boolFlags := []string{"--low-end", "--headless", "--wizard", "--skip-image",
		"--skip-attestation", "--require-attestation", "--skip-reachability"}
	for _, vf := range valueFlags {
		for _, args := range [][]string{
			{"serve", vf.flag, vf.value},
			{"serve", vf.flag + "=" + vf.value},
		} {
			if _, _, err := parseArgs(args); err != nil {
				t.Errorf("parseArgs(%v): %v", args, err)
			}
		}
	}
	for _, bf := range boolFlags {
		for _, args := range [][]string{
			{"serve", bf},
			{"serve", bf + "=false"},
			{"serve", bf + "=true"},
		} {
			if _, _, err := parseArgs(args); err != nil {
				t.Errorf("parseArgs(%v): %v", args, err)
			}
		}
	}
}

// TestRunEvalFeedbackFlags is the run-level proof for CHECKLIST
// 271-272: `eval-feedback --feedback-db` reaches the handler end
// to end through run(), not just parseArgs.
func TestRunEvalFeedbackFlags(t *testing.T) {
	isolateUserDirs(t)
	ctx := context.Background()

	// A store with real recorded traffic: 4 decisions, accepts
	// and discards, so Evaluate has a train/test split to A/B.
	dbPath := filepath.Join(t.TempDir(), "feedback.db")
	fb, err := feedback.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, sig := range []feedback.Signal{feedback.SignalAccept, feedback.SignalDiscard, feedback.SignalAccept, feedback.SignalDiscard} {
		id := fmt.Sprintf("d%d", i)
		if err := fb.RegisterDecision(ctx, feedback.DecisionRef{ID: id, Tier: "chat", Model: "m", TierScores: map[string]float64{"chat": 1}}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fb.RecordSignal(ctx, id, sig); err != nil {
			t.Fatal(err)
		}
	}
	if err := fb.Close(); err != nil {
		t.Fatal(err)
	}

	// The custom store is A/B'd (not the config default, which
	// does not even exist under this isolated HOME).
	var code int
	out := captureStdout(t, func() {
		code = run([]string{"eval-feedback", "--feedback-db", dbPath})
	})
	if code != 0 {
		t.Fatalf("eval-feedback exit = %d, want 0 (output %q)", code, out)
	}
	if !strings.Contains(out, "eval-feedback: cutoff") || !strings.Contains(out, "verdict:") {
		t.Fatalf("eval-feedback output = %q, want cutoff + verdict lines", out)
	}

	// = form + an explicit --cutoff in the past (all signals test).
	out = captureStdout(t, func() {
		code = run([]string{"eval-feedback", "--feedback-db=" + dbPath, "--cutoff=2020-01-01T00:00:00Z"})
	})
	if code != 0 {
		t.Fatalf("eval-feedback --cutoff exit = %d, want 0 (output %q)", code, out)
	}
	if !strings.Contains(out, "eval-feedback: cutoff 2020-01-01T00:00:00Z") {
		t.Fatalf("eval-feedback --cutoff output = %q, want the given cutoff", out)
	}

	// A missing store at the custom path reports no-evidence and
	// names that path — proof the flag value was honoured.
	missing := filepath.Join(t.TempDir(), "nope.db")
	out = captureStdout(t, func() {
		code = run([]string{"eval-feedback", "--feedback-db", missing})
	})
	if code != 0 {
		t.Fatalf("eval-feedback missing store exit = %d, want 0 (output %q)", code, out)
	}
	if !strings.Contains(out, "no feedback recorded yet (no store at "+missing+")") {
		t.Fatalf("eval-feedback missing store output = %q, want the custom path named", out)
	}

	// Bad cutoff is a usage error; a stray positional is rejected.
	if code := run([]string{"eval-feedback", "--feedback-db", dbPath, "--cutoff", "junk"}); code != 2 {
		t.Fatalf("eval-feedback bad cutoff exit = %d, want 2", code)
	}
	if code := run([]string{"eval-feedback", "stray"}); code != 2 {
		t.Fatalf("eval-feedback stray arg exit = %d, want 2", code)
	}
}

func TestPairQuotaCacheCachesWithinTTL(t *testing.T) {
	calls := 0
	query := func(ctx context.Context, p, m string, now time.Time) (ledger.PairQuota, error) {
		calls++
		return ledger.PairQuota{Provider: p, Model: m, Spent5h: float64(calls)}, nil
	}
	c := newPairQuotaCache(time.Second, query)
	ctx := context.Background()
	now := time.Now()
	first, ok := c.get(ctx, "prov", "model", now)
	if !ok {
		t.Fatal("first get: ok = false, want true")
	}
	// Within the TTL the query must not run again, even though the
	// underlying value would have changed (calls would be 2).
	second, ok := c.get(ctx, "prov", "model", now.Add(10*time.Millisecond))
	if !ok {
		t.Fatal("second get: ok = false, want true")
	}
	if calls != 1 {
		t.Fatalf("query calls after cached get = %d, want 1", calls)
	}
	if second.Spent5h != first.Spent5h {
		t.Fatalf("cached Spent5h = %v, want %v (first call's value)", second.Spent5h, first.Spent5h)
	}
	// A different pair is a different cache key.
	if _, ok := c.get(ctx, "other", "model", now); !ok || calls != 2 {
		t.Fatalf("other pair: ok = %v, calls = %d, want true / 2", ok, calls)
	}
	// After the TTL the entry expires and the query runs again.
	third, ok := c.get(ctx, "prov", "model", now.Add(2*time.Second))
	if !ok {
		t.Fatal("post-TTL get: ok = false, want true")
	}
	if calls != 3 {
		t.Fatalf("query calls after TTL expiry = %d, want 3", calls)
	}
	if third.Spent5h != 3 {
		t.Fatalf("post-TTL Spent5h = %v, want 3 (fresh query)", third.Spent5h)
	}
}

func TestPairQuotaCacheDoesNotCacheErrors(t *testing.T) {
	calls := 0
	query := func(ctx context.Context, p, m string, now time.Time) (ledger.PairQuota, error) {
		calls++
		if calls == 1 {
			return ledger.PairQuota{}, fmt.Errorf("boom")
		}
		return ledger.PairQuota{Provider: p, Model: m, Spent5h: 7}, nil
	}
	c := newPairQuotaCache(time.Second, query)
	ctx := context.Background()
	now := time.Now()
	if _, ok := c.get(ctx, "prov", "model", now); ok {
		t.Fatal("error get: ok = true, want false")
	}
	// The failed query must not be cached: the next call retries.
	got, ok := c.get(ctx, "prov", "model", now.Add(time.Millisecond))
	if !ok {
		t.Fatal("retry get: ok = false, want true")
	}
	if calls != 2 || got.Spent5h != 7 {
		t.Fatalf("retry: calls = %d, Spent5h = %v, want 2 / 7", calls, got.Spent5h)
	}
}

// TestApplyNadirConfig is the direct unit test for the Nadir wiring
// (M3 Area 1, PARITY row 396; S3 review nit): the helper behind the
// routing.nadir -> router.Config wiring in openStack. Sentinel
// values prove which fields a case leaves untouched.
func TestApplyNadirConfig(t *testing.T) {
	newCfg := func() router.Config {
		return router.Config{
			NadirEndpoint:  "preset-endpoint",
			NadirTimeoutMs: 4242,
			NadirAPIKey:    "preset-key",
		}
	}

	t.Run("nil config leaves engine untouched", func(t *testing.T) {
		// Env set on purpose: even with a key present, a nil
		// block must not touch any field (the engine is
		// fail-closed without an endpoint, so the key is dead).
		t.Setenv("CODVIAN_ROUTER_NADIR_API_KEY", "env-key")
		got := newCfg()
		applyNadirConfig(nil, &got)
		want := newCfg()
		if got.NadirEndpoint != want.NadirEndpoint || got.NadirTimeoutMs != want.NadirTimeoutMs || got.NadirAPIKey != want.NadirAPIKey {
			t.Fatalf("nil config: got endpoint=%q timeout=%d key=%q, want untouched endpoint=%q timeout=%d key=%q",
				got.NadirEndpoint, got.NadirTimeoutMs, got.NadirAPIKey,
				want.NadirEndpoint, want.NadirTimeoutMs, want.NadirAPIKey)
		}
	})

	t.Run("endpoint set, zero timeout keeps engine default", func(t *testing.T) {
		t.Setenv("CODVIAN_ROUTER_NADIR_API_KEY", "env-key")
		got := newCfg()
		applyNadirConfig(&config.NadirConfig{Endpoint: "https://api.getnadir.com", TimeoutMs: 0}, &got)
		if got.NadirEndpoint != "https://api.getnadir.com" {
			t.Fatalf("NadirEndpoint = %q, want %q", got.NadirEndpoint, "https://api.getnadir.com")
		}
		if got.NadirTimeoutMs != 4242 {
			t.Fatalf("NadirTimeoutMs = %d, want 4242 (untouched)", got.NadirTimeoutMs)
		}
		if got.NadirAPIKey != "env-key" {
			t.Fatalf("NadirAPIKey = %q, want %q", got.NadirAPIKey, "env-key")
		}
	})

	t.Run("non-zero timeout set, env key absent clears key", func(t *testing.T) {
		t.Setenv("CODVIAN_ROUTER_NADIR_API_KEY", "")
		got := newCfg()
		applyNadirConfig(&config.NadirConfig{Endpoint: "https://nadir.example", TimeoutMs: 5000}, &got)
		if got.NadirEndpoint != "https://nadir.example" {
			t.Fatalf("NadirEndpoint = %q, want %q", got.NadirEndpoint, "https://nadir.example")
		}
		if got.NadirTimeoutMs != 5000 {
			t.Fatalf("NadirTimeoutMs = %d, want 5000", got.NadirTimeoutMs)
		}
		if got.NadirAPIKey != "" {
			t.Fatalf("NadirAPIKey = %q, want empty (no env key)", got.NadirAPIKey)
		}
	})

	t.Run("present block with empty endpoint clears endpoint", func(t *testing.T) {
		t.Setenv("CODVIAN_ROUTER_NADIR_API_KEY", "env-key")
		got := newCfg()
		applyNadirConfig(&config.NadirConfig{TimeoutMs: 1000}, &got)
		if got.NadirEndpoint != "" {
			t.Fatalf("NadirEndpoint = %q, want empty (block assigns it)", got.NadirEndpoint)
		}
		if got.NadirTimeoutMs != 1000 {
			t.Fatalf("NadirTimeoutMs = %d, want 1000", got.NadirTimeoutMs)
		}
		if got.NadirAPIKey != "env-key" {
			t.Fatalf("NadirAPIKey = %q, want %q", got.NadirAPIKey, "env-key")
		}
	})
}
