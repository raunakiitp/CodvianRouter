package main

import (
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

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
