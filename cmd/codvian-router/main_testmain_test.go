package main

import (
	"os"
	"testing"
)

// TestMain opts the CLI tests into local provider endpoints. The
// provider-management subcommands are exercised against local
// httptest mocks (a loopback base URL), which the SSRF guard
// rejects by default — that default is the production behaviour
// and is deliberate. The opt-in here is test-only and keeps every
// assertion (the CLI still validates keys against the mock
// upstream) while leaving the shipped default secure.
func TestMain(m *testing.M) {
	_ = os.Setenv("CODVIAN_ROUTER_ALLOW_PRIVATE_ENDPOINTS", "1")
	os.Exit(m.Run())
}
