// W1.4: the bootstrap key is the local operator's first key —
// without admin scope a fresh install's dashboard cannot call
// admin routes, so the bootstrap issuer must grant it.
package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

func TestW1BootstrapKeyHasAdminScope(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig(dir)
	if err := cfg.EnsureDataDir(); err != nil {
		t.Fatalf("EnsureDataDir: %v", err)
	}
	issuer := bootstrapIssuer(cfg, filepath.Join(dir, "config.json"))
	raw, err := issuer(context.Background())
	if err != nil {
		t.Fatalf("bootstrap issuer: %v", err)
	}
	if raw == "" {
		t.Fatal("bootstrap issuer returned an empty raw key")
	}
	if len(cfg.GatewayKeys) != 1 {
		t.Fatalf("gateway keys = %d, want 1", len(cfg.GatewayKeys))
	}
	for _, want := range []string{"chat", "models", "admin"} {
		found := false
		for _, s := range cfg.GatewayKeys[0].Scopes {
			if s == want {
				found = true
			}
		}
		if !found {
			t.Errorf("bootstrap key scopes = %v, want %q included", cfg.GatewayKeys[0].Scopes, want)
		}
	}
}
