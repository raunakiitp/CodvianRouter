package main

// A3 cross-cutting regression (stream C): the per-provider OAuth handler
// registry must be wired into PRODUCTION, not only reachable from tests.
// PR #117/#119 built the registry and the server bridge
// (internal/server/oauth_provider_bridge.go); if the main.go wiring is
// ever reverted to the legacy token-resolver call, this test fails —
// the bridge would be dead code and every catalog OAuth provider would
// silently lose its per-provider refresh/egress behavior.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestProductionMainWiresHandlerOAuthResolver(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	var installed bool
	var legacy []string
	for _, ln := range strings.Split(string(data), "\n") {
		code := strings.TrimSpace(ln)
		if code == "" || strings.HasPrefix(code, "//") {
			continue
		}
		if strings.Contains(code, "InstallOAuthProviderResolver(") {
			installed = true
		}
		// The pre-A3 wiring: the registry's token resolver pointed
		// straight at the legacy dashboard-store token lookup. The
		// handler-based resolver (InstallOAuthProviderResolver) is the
		// production path; this call must not survive anywhere in main.go.
		if strings.Contains(code, "SetOAuthTokenResolver(") && strings.Contains(code, "OAuthAccessToken") {
			legacy = append(legacy, code)
		}
	}

	if len(legacy) > 0 {
		t.Fatalf("production main.go wires the legacy OAuth token resolver directly — the per-provider handler resolver is bypassed:\n  %s",
			strings.Join(legacy, "\n  "))
	}
	if !installed {
		t.Fatal("production main.go never calls InstallOAuthProviderResolver — the per-provider OAuth handler registry is not wired into production")
	}
}
