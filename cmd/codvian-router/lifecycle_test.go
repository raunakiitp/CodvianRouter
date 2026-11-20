package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/raunakiitp/codvian-router/internal/tray"
)

func TestParseArgsLifecycleFlags(t *testing.T) {
	opts, pos, err := parseArgs([]string{"logs", "--lines", "10", "--follow"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.logsLines != 10 || !opts.logsLinesSet || !opts.logsFollow {
		t.Fatalf("opts = %+v", opts)
	}
	if len(pos) != 1 || pos[0] != "logs" {
		t.Fatalf("positional = %v", pos)
	}

	opts, pos, err = parseArgs([]string{"update", "--tag=v9.9.9", "--check=false", "--yes"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.updateTag != "v9.9.9" || opts.updateCheck || !opts.updateYes {
		t.Fatalf("opts = %+v", opts)
	}
	if len(pos) != 1 || pos[0] != "update" {
		t.Fatalf("positional = %v", pos)
	}

	opts, _, err = parseArgs([]string{"logs", "--lines=0"})
	if err != nil {
		t.Fatalf("parseArgs: %v", err)
	}
	if opts.logsLines != 0 || !opts.logsLinesSet {
		t.Fatalf("opts = %+v", opts)
	}

	if _, _, err := parseArgs([]string{"logs", "--lines", "-1"}); err == nil {
		t.Fatal("expected error for --lines -1")
	}
	if _, _, err := parseArgs([]string{"logs", "--lines=abc"}); err == nil {
		t.Fatal("expected error for --lines=abc")
	}
	if _, _, err := parseArgs([]string{"update", "--check=maybe"}); err == nil {
		t.Fatal("expected error for --check=maybe")
	}
}

func TestReadSupervisorPIDFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.pid")
	if _, err := readSupervisorPIDFile(path); !os.IsNotExist(err) {
		t.Fatalf("missing file: err = %v, want IsNotExist", err)
	}
	if err := os.WriteFile(path, []byte(`{"supervisor_pid": 111, "gateway_pid": 222, "port": 20128}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec, err := readSupervisorPIDFile(path)
	if err != nil {
		t.Fatalf("readSupervisorPIDFile: %v", err)
	}
	if rec.SupervisorPID != 111 || rec.GatewayPID != 222 || rec.Port != 20128 {
		t.Fatalf("rec = %+v", rec)
	}
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSupervisorPIDFile(path); err == nil {
		t.Fatal("expected error for corrupt PID file")
	}
	if err := os.WriteFile(path, []byte(`{"supervisor_pid": 111, "gateway_pid": 0, "port": 20128}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSupervisorPIDFile(path); err == nil {
		t.Fatal("expected error for PID file with no gateway pid")
	}
}

func TestProbeHealthz(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	probe := probeHealthz(port)
	if !probe.ok || probe.status != 200 || probe.body != `{"status":"ok"}` {
		t.Fatalf("probe = %+v", probe)
	}
	if probe.rtt <= 0 {
		t.Fatalf("probe.rtt = %v, want > 0", probe.rtt)
	}

	srv.Close()
	probe = probeHealthz(port)
	if probe.ok || probe.err == nil {
		t.Fatalf("closed server: probe = %+v, want an error", probe)
	}

	srv500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "boom")
	}))
	defer srv500.Close()
	probe = probeHealthz(srv500.Listener.Addr().(*net.TCPAddr).Port)
	if probe.ok || probe.status != 500 {
		t.Fatalf("500 server: probe = %+v", probe)
	}
}

func TestTailLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.log")
	if _, err := tailLogFile(path, 10); !os.IsNotExist(err) {
		t.Fatalf("missing file: err = %v, want IsNotExist", err)
	}
	lines := []string{"one", "two", "three", "four", "five"}
	var buf bytes.Buffer
	for _, l := range lines {
		buf.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := tailLogFile(path, 2)
	if err != nil {
		t.Fatalf("tailLogFile: %v", err)
	}
	if len(got) != 2 || got[0] != "four" || got[1] != "five" {
		t.Fatalf("tail 2 = %v", got)
	}
	got, err = tailLogFile(path, 0)
	if err != nil {
		t.Fatalf("tailLogFile(0): %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("tail 0 = %v, want all 5 lines", got)
	}
	got, err = tailLogFile(path, 50)
	if err != nil || len(got) != 5 {
		t.Fatalf("tail 50 = %v, err = %v", got, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = tailLogFile(path, 10)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty file: got = %v, err = %v", got, err)
	}
}

func TestFollowLogFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "router.log")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- followLogFile(ctx, path, &out) }()
	time.Sleep(600 * time.Millisecond)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new-line\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	time.Sleep(700 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("followLogFile: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("followLogFile did not return after cancel")
	}
	if !bytes.Contains(out.Bytes(), []byte("new-line")) {
		t.Fatalf("follow output = %q, want it to contain new-line", out.String())
	}
	if bytes.Contains(out.Bytes(), []byte("old")) {
		t.Fatalf("follow output = %q, want only appended lines, not the pre-existing tail", out.String())
	}
}

// freeClosedPort returns a localhost port with nothing listening on
// it (bind :0, read the port, close).
func freeClosedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

func TestRunStatusNoGateway(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	port := freeClosedPort(t)
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "status"}); code != 1 {
		t.Fatalf("status with no gateway: exit = %d, want 1", code)
	}
}

func TestRunHealthNoGateway(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	port := freeClosedPort(t)
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "health"}); code != 1 {
		t.Fatalf("health with no gateway: exit = %d, want 1", code)
	}
}

func TestRunStopNoGateway(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	port := freeClosedPort(t)
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "stop"}); code != 0 {
		t.Fatalf("stop with no gateway: exit = %d, want 0 (nothing to stop)", code)
	}
}

// healthzServer starts a server that answers /healthz 200 on a
// localhost port, standing in for a gateway started directly with
// `serve` (no supervisor PID file).
func healthzServer(t *testing.T) (port int, close func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"status":"ok"}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	return srv.Listener.Addr().(*net.TCPAddr).Port, srv.Close
}

func TestRunStatusUnmanagedGateway(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	port, close := healthzServer(t)
	defer close()
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "status"}); code != 0 {
		t.Fatalf("status with an unmanaged gateway answering /healthz: exit = %d, want 0", code)
	}
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "health"}); code != 0 {
		t.Fatalf("health with an unmanaged gateway: exit = %d, want 0", code)
	}
}

func TestRunStopRefusesUnmanagedGateway(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	port, close := healthzServer(t)
	defer close()
	// stop must not signal a process it cannot prove is a
	// supervised gateway: exit 1, and the server keeps running.
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "stop"}); code != 1 {
		t.Fatalf("stop with an unmanaged gateway: exit = %d, want 1", code)
	}
	if probe := probeHealthz(port); !probe.ok {
		t.Fatalf("unmanaged gateway stopped answering /healthz after refused stop: %+v", probe)
	}
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "restart"}); code != 1 {
		t.Fatalf("restart with an unmanaged gateway: exit = %d, want 1", code)
	}
}

func TestRunStopStalePIDFile(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	// A pid that is certainly dead: start a sleep, wait for it.
	cmd := exec.Command("sleep", "0.05")
	if err := cmd.Start(); err != nil {
		t.Skipf("sleep not available: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Wait()
	if processAlive(deadPID) {
		t.Fatalf("test setup: pid %d still alive", deadPID)
	}
	pidPath := tray.PIDFilePath(cfg)
	rec := fmt.Sprintf(`{"supervisor_pid": %d, "gateway_pid": %d, "port": 20128}`+"\n", deadPID, deadPID)
	if err := os.WriteFile(pidPath, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	port := freeClosedPort(t)
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "stop"}); code != 0 {
		t.Fatalf("stop with a stale PID file: exit = %d, want 0", code)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("stale PID file %s was not removed", pidPath)
	}
	// And status agrees the gateway is not running.
	if code := run([]string{"--config", cfg, "--port", fmt.Sprint(port), "status"}); code != 1 {
		t.Fatalf("status after stale PID file removal: exit = %d, want 1", code)
	}
}

func TestRunLogsMissingAndTail(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if code := run([]string{"--config", cfg, "logs"}); code != 1 {
		t.Fatalf("logs with no log file: exit = %d, want 1", code)
	}
	logPath := tray.LogFilePath(cfg)
	var buf bytes.Buffer
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&buf, "line %d\n", i)
	}
	if err := os.WriteFile(logPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"--config", cfg, "logs", "--lines", "2"}); code != 0 {
		t.Fatalf("logs with a log file: exit = %d, want 0", code)
	}
	if code := run([]string{"--config", cfg, "logs", "extra"}); code != 2 {
		t.Fatalf("logs with an extra argument: exit = %d, want 2", code)
	}
}

func TestVerifyRouterProcess(t *testing.T) {
	if verifyRouterProcess(-1) || verifyRouterProcess(0) || verifyRouterProcess(999999) {
		t.Fatal("verifyRouterProcess accepted a bogus pid")
	}
	// PID 1 is the container init, not a codvian-router process.
	if verifyRouterProcess(1) {
		t.Fatal("verifyRouterProcess(1) = true, want false (init is not codvian-router)")
	}
}

func TestRunUnknownLifecycleArgs(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	if code := run([]string{"--config", cfg, "status", "extra"}); code != 2 {
		t.Fatalf("status with an extra argument: exit = %d, want 2", code)
	}
	if code := run([]string{"--config", cfg, "update", "v1", "v2"}); code != 2 {
		t.Fatalf("update with two tags: exit = %d, want 2", code)
	}
}
