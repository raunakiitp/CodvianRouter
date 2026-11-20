package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/guardrails"
)

// TestGuardrailsFeedListsTypesAndToggle proves the per-type toggle
// (B3-17) works end to end through the dashboard feed: GET
// /dashboard/guardrails lists every built-in pattern label with its
// enabled state, POST disabled_types flips one off, the next GET
// shows it off, and Check drops that type's findings while other
// types keep matching. The dashboard renders its switches from this
// feed — without the types list the switches never render at all.
func TestGuardrailsFeedListsTypesAndToggle(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})

	getTypes := func(t *testing.T) map[string]guardrails.TypeState {
		t.Helper()
		rec := doReq(t, srv, http.MethodGet, "/dashboard/guardrails", "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("guardrails feed status = %d, want 200", rec.Code)
		}
		var feed struct {
			Types []guardrails.TypeState `json:"types"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &feed); err != nil {
			t.Fatalf("guardrails feed is not JSON: %v", err)
		}
		out := make(map[string]guardrails.TypeState, len(feed.Types))
		for _, ts := range feed.Types {
			out[ts.Pattern] = ts
		}
		return out
	}

	// Baseline: the email type is listed and enabled, and Check
	// actually finds an email address.
	types := getTypes(t)
	email, ok := types["email"]
	if !ok {
		t.Fatalf("feed types list has no \"email\" entry (got %d types)", len(types))
	}
	if email.Kind != "pii" || !email.Enabled {
		t.Fatalf("feed email type = %+v, want kind pii enabled true", email)
	}
	if len(types) < 2 {
		t.Fatalf("feed lists %d types, want at least 2", len(types))
	}
	res := srv.guardrails.Check("contact me at a@b.com", "test")
	if !hasFinding(res, "email") {
		t.Fatalf("Check before toggle: no email finding in %+v", res.Findings)
	}

	// Disable the email type through the same POST the dashboard
	// switch sends.
	rec := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"disabled_types":["email"]}`, "test-key-123")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST disabled_types status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// The feed now shows email off and the other types still on,
	// and Check drops email findings.
	types = getTypes(t)
	if types["email"].Enabled {
		t.Fatalf("feed email type still enabled after POST disabled_types")
	}
	for pattern, ts := range types {
		if pattern != "email" && !ts.Enabled {
			t.Fatalf("feed type %q disabled, want only email disabled", pattern)
		}
	}
	res = srv.guardrails.Check("contact me at a@b.com", "test")
	if hasFinding(res, "email") {
		t.Fatalf("Check after toggle: email finding still present in %+v", res.Findings)
	}

	// An unknown type name is a 400 naming the type — a typo must
	// fail loudly, never silently leave the operator believing a
	// type is toggled.
	bad := doReq(t, srv, http.MethodPost, "/dashboard/guardrails",
		`{"disabled_types":["no-such-type"]}`, "test-key-123")
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("POST unknown disabled_type status = %d, want 400 (body %q)", bad.Code, bad.Body.String())
	}
}

func hasFinding(res guardrails.Result, pattern string) bool {
	for _, f := range res.Findings {
		if f.Pattern == pattern {
			return true
		}
	}
	return false
}
