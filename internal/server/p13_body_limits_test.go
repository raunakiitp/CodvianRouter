// AUD-SEC-003 regression tests: control-payload handlers must bound the
// request body before decoding it.
//
// Before the fix, seven handlers decoded r.Body with no limit, so a
// client could stream an unbounded body into memory. Every other handler
// wraps r.Body in http.MaxBytesReader first.
package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
)

// p13OversizedJSON builds a valid JSON object larger than the cap.
func p13OversizedJSON() string {
	return `{"name":"x","pad":"` + strings.Repeat("p", maxAdminJSONBody+1024) + `"}`
}

func p13AdminServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := config.DefaultConfig(t.TempDir())
	_, rawAdmin := issueScopedKey(t, cfg, "p13-sec003-admin", []string{"admin", "chat", "management"})
	srv := newTestServer(t, &fakeEngine{}, WithConfig(cfg))
	return srv, rawAdmin
}

func TestAUD_SEC_003OversizedControlBodiesAreRefused(t *testing.T) {
	srv, rawAdmin := p13AdminServer(t)
	big := p13OversizedJSON()

	// NOTE: /api/fallback/chains is bounded in code too, but this
	// harness has no fallback-chain store, so the handler answers 503
	// before it ever decodes a body — it cannot prove the limit here.
	for _, path := range []string{"/api/org", "/api/eval/schedules"} {
		t.Run(path, func(t *testing.T) {
			rec := doLocalReq(t, srv, http.MethodPost, path, big, rawAdmin)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("oversized body to %s answered %d, want 400 from the bounded decode: %s", path, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "invalid request body") && !strings.Contains(rec.Body.String(), "too large") {
				t.Fatalf("oversized body to %s was refused by something other than the body limit: %s", path, rec.Body.String())
			}
			// A size refusal must never look like a successful create.
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil {
				if _, ok := body["id"]; ok {
					t.Fatalf("oversized body to %s produced a created object: %s", path, rec.Body.String())
				}
			}
		})
	}
}

// TestAUD_SEC_003NormalPayloadsStillWork: the cap must not break real
// traffic.
func TestAUD_SEC_003NormalPayloadsStillWork(t *testing.T) {
	srv, rawAdmin := p13AdminServer(t)
	rec := doLocalReq(t, srv, http.MethodPost, "/api/org", `{"name":"eng"}`, rawAdmin)
	if rec.Code >= 500 {
		t.Fatalf("small payload to /api/org answered %d: %s", rec.Code, rec.Body.String())
	}
}
