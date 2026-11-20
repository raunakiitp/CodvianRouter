// W1.5: the keys feed surfaces the Disabled flag (M1.3 kill
// switch) so the dashboard can show a key's disabled state.
package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

func TestW1KeysFeedIncludesDisabled(t *testing.T) {
	cfg := config.DefaultConfig(t.TempDir())
	enabled, _, err := config.IssueKey("active", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("IssueKey(active): %v", err)
	}
	disabled, _, err := config.IssueKey("revoked", []string{"chat"}, 0)
	if err != nil {
		t.Fatalf("IssueKey(revoked): %v", err)
	}
	disabled.Disabled = true
	for _, k := range []config.GatewayKey{enabled, disabled} {
		if err := cfg.AddGatewayKey(k); err != nil {
			t.Fatalf("AddGatewayKey: %v", err)
		}
	}
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	var keys routerKeysResponse
	getJSON(t, srv, "/dashboard/keys", &keys)
	if keys.Total != 2 || len(keys.Keys) != 2 {
		t.Fatalf("keys total = %d, want 2", keys.Total)
	}
	byName := map[string]routerKeyView{}
	for _, k := range keys.Keys {
		byName[k.Name] = k
	}
	if byName["active"].Disabled {
		t.Errorf("active key: disabled = true, want false")
	}
	if !byName["revoked"].Disabled {
		t.Errorf("revoked key: disabled = false, want true")
	}
	// The raw feed body must carry the flag explicitly for the
	// disabled key (not just a zero-value omission).
	rec := doReq(t, srv, http.MethodGet, "/dashboard/keys", "", "")
	if body := rec.Body.String(); !strings.Contains(body, `"disabled":true`) {
		t.Errorf("keys feed body has no \"disabled\":true (body %s)", body)
	}
}
