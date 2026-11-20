package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockUpstream serves the two endpoints the registry's key test and
// model fetch need: /v1/models and /v1/chat/completions.
func mockUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/v1/chat/completions":
			_, _ = w.Write([]byte(`{"model":"model-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestParseArgsConfigureFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"configure", "--provider", "groq", "--api-key-env", "GROQ_KEY"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.providerID != "groq" || opts.apiKeyEnv != "GROQ_KEY" || len(pos) != 1 || pos[0] != "configure" {
		t.Fatalf("opts = %+v, positional = %v", opts, pos)
	}
	opts, _, err = parseArgs([]string{"configure", "--provider=groq", "--api-key=secret-key"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.providerID != "groq" || opts.apiKey != "secret-key" {
		t.Fatalf("opts = %+v", opts)
	}
	if _, _, err := parseArgs([]string{"configure", "--provider"}); err == nil {
		t.Fatal("expected error for --provider without a value")
	}
	if _, _, err := parseArgs([]string{"configure", "--api-key-env"}); err == nil {
		t.Fatal("expected error for --api-key-env without a value")
	}
}

func TestRunDoctorNoConfigFails(t *testing.T) {
	isolateUserDirs(t)
	var out bytes.Buffer
	if code := runDoctorTo(options{}, &out); code != 1 {
		t.Fatalf("doctor without a config exit = %d, want 1\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "[FAIL] config") {
		t.Errorf("doctor output does not report the missing config:\n%s", out.String())
	}
}

func TestRunDoctorFreshInitPasses(t *testing.T) {
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	var out bytes.Buffer
	if code := runDoctorTo(options{}, &out); code != 0 {
		t.Fatalf("doctor on a fresh init exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"[PASS] config",
		"[PASS] gateway keys",
		"[PASS] data dir",
		"[WARN] providers",
		"[PASS] listen port",
		"[WARN] dashboard",
		"doctor: ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor output missing %q:\n%s", want, got)
		}
	}
}

func TestRunDoctorPortFlagOverridesListen(t *testing.T) {
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	// The config says 20128; --port 20197 must be the address the
	// port/dashboard checks probe instead.
	var out bytes.Buffer
	if code := runDoctorTo(options{port: 20197}, &out); code != 0 {
		t.Fatalf("doctor --port exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "[PASS] listen port — 127.0.0.1:20197 is free") {
		t.Errorf("doctor --port did not probe 20197:\n%s", got)
	}
	if strings.Contains(got, "20128") {
		t.Errorf("doctor --port still probed the config port 20128:\n%s", got)
	}
}

func TestRunDoctorWithMockProvider(t *testing.T) {
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	mock := mockUpstream(t)
	defer mock.Close()
	if code := run([]string{"provider", "add-catalog", "fireworks", "fw-test-key",
		"--endpoint", "chat", "--variant", "alt-header", "--base-url", mock.URL}); code != 0 {
		t.Fatalf("provider add-catalog exit = %d, want 0", code)
	}
	var out bytes.Buffer
	if code := runDoctorTo(options{}, &out); code != 0 {
		t.Fatalf("doctor with a live mock provider exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "[PASS] provider") || !strings.Contains(got, "reachability") {
		t.Errorf("doctor output does not report the live provider probe:\n%s", got)
	}
	// The key itself must never appear in doctor output.
	if strings.Contains(got, "fw-test-key") {
		t.Errorf("doctor output leaked the provider key:\n%s", got)
	}
}

func TestRunConfigureCreatesConfigFromPipedInput(t *testing.T) {
	isolateUserDirs(t)
	// Empty port answer (keep default) + empty provider (skip).
	in := strings.NewReader("\n\n")
	var out bytes.Buffer
	if code := runConfigureIO(options{}, in, &out); code != 0 {
		t.Fatalf("configure exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{"created config:", "listening on 127.0.0.1:20128", "gateway keys: 1", "dashboard: http://127.0.0.1:20128/dashboard"} {
		if !strings.Contains(got, want) {
			t.Errorf("configure output missing %q:\n%s", want, got)
		}
	}
	// Second run is idempotent: no new key, no duplicate config.
	in = strings.NewReader("\n\n")
	out.Reset()
	if code := runConfigureIO(options{}, in, &out); code != 0 {
		t.Fatalf("second configure exit = %d, want 0\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "created config:") {
		t.Errorf("second configure recreated the config:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "gateway keys: 1") {
		t.Errorf("second configure changed the key count:\n%s", out.String())
	}
}

func TestRunConfigureFlagProviderWithMock(t *testing.T) {
	isolateUserDirs(t)
	mock := mockUpstream(t)
	defer mock.Close()
	opts := options{providerID: "fireworks", apiKey: "fw-test-key", baseURL: mock.URL}
	var out bytes.Buffer
	if code := runConfigureIO(opts, strings.NewReader(""), &out); code != 0 {
		t.Fatalf("configure --provider exit = %d, want 0\n%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "added provider") || !strings.Contains(got, "providers: 1 configured") {
		t.Errorf("configure output does not report the added provider:\n%s", got)
	}
	if strings.Contains(got, "fw-test-key") {
		t.Errorf("configure output leaked the provider key:\n%s", got)
	}
}

func TestRunConfigureUsageErrors(t *testing.T) {
	isolateUserDirs(t)
	var out bytes.Buffer
	// Provider without a key.
	if code := runConfigureIO(options{providerID: "groq"}, strings.NewReader(""), &out); code != 2 {
		t.Fatalf("configure --provider without key exit = %d, want 2", code)
	}
	// Key without a provider.
	out.Reset()
	if code := runConfigureIO(options{apiKey: "some-key"}, strings.NewReader(""), &out); code != 2 {
		t.Fatalf("configure --api-key without provider exit = %d, want 2", code)
	}
	// Unknown catalog id.
	out.Reset()
	if code := runConfigureIO(options{providerID: "no-such-provider", apiKey: "k"}, strings.NewReader(""), &out); code != 1 {
		t.Fatalf("configure unknown provider exit = %d, want 1", code)
	}
	// --api-key-env naming an unset variable.
	out.Reset()
	if code := runConfigureIO(options{providerID: "groq", apiKeyEnv: "CODVIAN_ROUTER_TEST_UNSET_KEY"}, strings.NewReader(""), &out); code != 2 {
		t.Fatalf("configure unset --api-key-env exit = %d, want 2", code)
	}
}

func TestRunMCPRequiresConfig(t *testing.T) {
	isolateUserDirs(t)
	var out bytes.Buffer
	if code := runMCPTo(options{}, strings.NewReader(""), &out); code != 1 {
		t.Fatalf("mcp without a config exit = %d, want 1", code)
	}
}

func TestRunMCPToolsCallLiveData(t *testing.T) {
	isolateUserDirs(t)
	if code := run([]string{"init"}); code != 0 {
		t.Fatalf("init exit = %d, want 0", code)
	}
	mock := mockUpstream(t)
	defer mock.Close()
	if code := run([]string{"provider", "add-catalog", "fireworks", "fw-test-key",
		"--endpoint", "chat", "--variant", "alt-header", "--base-url", mock.URL}); code != 0 {
		t.Fatalf("provider add-catalog exit = %d, want 0", code)
	}
	in := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{}}\n" +
			"{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"provider_list\",\"arguments\":{}}}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"model_list\",\"arguments\":{}}}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":4,\"method\":\"tools/call\",\"params\":{\"name\":\"router_status\",\"arguments\":{}}}\n")
	var out bytes.Buffer
	if code := runMCPTo(options{}, in, &out); code != 0 {
		t.Fatalf("mcp exit = %d, want 0", code)
	}
	got := out.String()
	// Four requests, one notification: exactly four response lines.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 4 {
		t.Fatalf("mcp wrote %d lines, want 4:\n%s", len(lines), got)
	}
	// Live data, not canned: the mock provider's model and the real
	// config's listen address must appear in the tool results.
	if !strings.Contains(got, "model-a") {
		t.Errorf("mcp model_list does not carry the live model:\n%s", got)
	}
	if !strings.Contains(got, "127.0.0.1:20128") {
		t.Errorf("mcp router_status does not carry the live listen address:\n%s", got)
	}
	if strings.Contains(got, "fw-test-key") || strings.Contains(got, "base_url") {
		t.Errorf("mcp output leaked a secret:\n%s", got)
	}
}
