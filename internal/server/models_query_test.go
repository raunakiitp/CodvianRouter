package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

func capabilityTestRegistry(t *testing.T) *provider.Registry {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	trueVal := true
	falseVal := false
	cfgs := []provider.ProviderConfig{
		{
			ID: "cap-prov", Name: "Capability Provider", BaseURL: "https://example.com/api",
			Models: []provider.Model{
				{ID: "vision-model", Vision: true, VisionKnown: true},
				{ID: "tool-model", Tools: &trueVal},
				{ID: "notool-model", Tools: &falseVal},
				{ID: "reason-model", Reasoning: true},
				{ID: "audio-model", Audio: true, AudioKnown: true},
				{ID: "video-model", Video: true, VideoKnown: true},
				{ID: "code-coder-model"},
				{ID: "plain-text-model"},
			},
			Enabled: true,
		},
	}
	raw, err := json.Marshal(cfgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry(provider.NewJSONFileStore(path))
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestModelsQueryCapabilityFiltering(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(capabilityTestRegistry(t)))
	mux := m810Mux(srv)

	tests := []struct {
		capability string
		wantModel  string
	}{
		{"vision", "vision-model"},
		{"tools", "tool-model"},
		{"reasoning", "reason-model"},
		{"audio", "audio-model"},
		{"video", "video-model"},
		{"code", "code-coder-model"},
	}

	for _, tc := range tests {
		t.Run(tc.capability, func(t *testing.T) {
			rec := m810Do(t, mux, http.MethodGet, "/v1/models/query?capability="+tc.capability)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp struct {
				Object string                `json:"object"`
				Data   []modelDetailResponse `json:"data"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if len(resp.Data) != 1 {
				t.Fatalf("expected 1 model, got %d: %+v", len(resp.Data), resp.Data)
			}
			if resp.Data[0].ID != tc.wantModel {
				t.Errorf("model = %q, want %q", resp.Data[0].ID, tc.wantModel)
			}
		})
	}
}

func TestModelsQueryInvalidCapability400(t *testing.T) {
	srv := newTestServer(t, &fakeEngine{}, WithProviderRegistry(capabilityTestRegistry(t)))
	mux := m810Mux(srv)

	rec := m810Do(t, mux, http.MethodGet, "/v1/models/query?capability=quantum_computing")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
