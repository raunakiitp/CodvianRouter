package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestOpenAPIConsistency is the OpenAPI consistency check
// (the reference router's x-loopback-only / x-always-protected scripts,
// research-log entry 143): the spec generated live from the
// guard route table must agree with the table exactly — every
// registered route present, no extra operations, and every
// extension matching the route's tier and spawn flag.
func TestOpenAPIConsistency(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithVersion("9.9.9-test"))
	rec := doReq(t, srv, "GET", "/openapi.json", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json: status %d, want 200", rec.Code)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Summary         string `json:"summary"`
			LoopbackOnly    *bool  `json:"x-loopback-only"`
			AlwaysProtected *bool  `json:"x-always-protected"`
			SpawnCapable    *bool  `json:"x-spawn-capable"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("openapi.json is not JSON: %v", err)
	}
	if spec.OpenAPI != "3.0.3" {
		t.Errorf("openapi = %q, want 3.0.3", spec.OpenAPI)
	}
	if spec.Info.Version != "9.9.9-test" {
		t.Errorf("info.version = %q, want the wired version", spec.Info.Version)
	}
	table := RouteTable()
	if len(spec.Paths) == 0 {
		t.Fatal("spec has no paths")
	}
	// Every table route: present, with the right extensions.
	seenOps := 0
	for _, route := range table {
		methods, ok := spec.Paths[route.Path]
		if !ok {
			t.Errorf("spec is missing path %s", route.Path)
			continue
		}
		op, ok := methods[strings.ToLower(route.Method)]
		if !ok {
			t.Errorf("spec path %s is missing method %s", route.Path, route.Method)
			continue
		}
		seenOps++
		if op.Summary == "" {
			t.Errorf("spec %s %s has no summary", route.Method, route.Path)
		}
		wantLoop := route.Tier == TierLocalOnly
		if (op.LoopbackOnly != nil && *op.LoopbackOnly) != wantLoop {
			t.Errorf("spec %s %s x-loopback-only = %v, want %v", route.Method, route.Path, op.LoopbackOnly, wantLoop)
		}
		wantProt := route.Tier == TierAlwaysProtected
		if (op.AlwaysProtected != nil && *op.AlwaysProtected) != wantProt {
			t.Errorf("spec %s %s x-always-protected = %v, want %v", route.Method, route.Path, op.AlwaysProtected, wantProt)
		}
		if (op.SpawnCapable != nil && *op.SpawnCapable) != route.SpawnCapable {
			t.Errorf("spec %s %s x-spawn-capable = %v, want %v", route.Method, route.Path, op.SpawnCapable, route.SpawnCapable)
		}
	}
	// No extra operations in the spec: every spec op must be a
	// table route (a stale spec entry would otherwise survive).
	for path, methods := range spec.Paths {
		for method := range methods {
			found := false
			for _, route := range table {
				if route.Path == path && strings.ToLower(route.Method) == method {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("spec has extra operation %s %s with no table route", method, path)
			}
		}
	}
	if seenOps != len(table) {
		t.Errorf("spec covers %d operations, table has %d routes", seenOps, len(table))
	}
}

// TestOpenAPIIsAuthExempt proves the spec is reachable without a
// key, like the other dashboard feeds — it documents the public
// surface and carries no secrets.
func TestOpenAPIIsAuthExempt(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{})
	rec := doReq(t, srv, "GET", "/openapi.json", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json without key: status %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "test-key") {
		t.Errorf("spec leaks test material: %s", rec.Body.String())
	}
}
