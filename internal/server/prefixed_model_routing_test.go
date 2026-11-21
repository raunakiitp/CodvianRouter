package server

// Nested model-ID addressing (provider-quirks audit slice, live
// OpenRouter verification 2026-11-21): aggregators list nested
// vendor/model IDs verbatim in /v1/models ("liquid/lfm-3-1.7b:free"),
// and their first segment is a vendor namespace — often also a
// catalog entry — not one of our providers. applyPrefixedModelRouting
// (B2-29) used to treat EVERY slash as a provider pin, so those
// listed IDs 400'd with `routing.only: unknown provider "liquid"`.
// The escape hatch: the prefix is only a pin when the named provider
// serves the BARE model; a full string that a registered provider
// serves verbatim stays a model ID. Explicit pins keep working.

import (
	"context"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// pinOffersKeyManager is the minimal ProviderKeyManager surface
// resolveProviderV1 uses (Get + List); the embedded nil interface is
// never called for anything else on this path.
type pinOffersKeyManager struct {
	ProviderKeyManager
	provs []*provider.Provider
}

func (m *pinOffersKeyManager) Get(id string) (*provider.Provider, bool) {
	for _, p := range m.provs {
		if p.ID == id {
			return p, true
		}
	}
	return nil, false
}

func (m *pinOffersKeyManager) List() []*provider.Provider { return m.provs }

// pinOffersAdder implements ProviderAdder plus the offer surface the
// escape hatch asserts. Its provider matching mirrors
// provider.Registry.ProviderOffersModel: registry ID first, then
// catalog ID / display name (resolveProviderV1 resolves catalog
// entries, whose IDs differ from the registry's generated IDs).
type pinOffersAdder struct{ provs []*provider.Provider }

func (a *pinOffersAdder) AddCatalogWithOptions(context.Context, string, string, provider.CatalogAddOptions) (*provider.Provider, error) {
	panic("pinOffersAdder: add not used by applyPrefixedModelRouting")
}

func (a *pinOffersAdder) AddContext(context.Context, string, string, string) (*provider.Provider, error) {
	panic("pinOffersAdder: add not used by applyPrefixedModelRouting")
}

func (a *pinOffersAdder) find(id string) *provider.Provider {
	for _, p := range a.provs {
		if p.ID == id {
			return p
		}
	}
	for _, p := range a.provs {
		if strings.EqualFold(p.CatalogID, id) || strings.EqualFold(p.Name, id) {
			return p
		}
	}
	return nil
}

func pinProviderOffers(p *provider.Provider, model string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	if len(p.Models) == 0 {
		return true
	}
	for _, m := range p.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

func (a *pinOffersAdder) ProviderOffersModel(providerID, modelID string) bool {
	return pinProviderOffers(a.find(providerID), modelID)
}

func (a *pinOffersAdder) OffersModel(modelID string) bool {
	for _, p := range a.provs {
		if pinProviderOffers(p, modelID) {
			return true
		}
	}
	return false
}

func pinTestProviders() []*provider.Provider {
	return []*provider.Provider{
		{
			ID: "prov_or", Name: "OpenRouter", CatalogID: "openrouter", Enabled: true,
			Models: []provider.Model{
				{ID: "liquid/lfm-3-1.7b:free"},
				{ID: "openai/gpt-4o"},
				{ID: "zzvendor/zzmodel"},
			},
		},
		{
			ID: "prov_oai", Name: "OpenAI", CatalogID: "openai", Enabled: true,
			Models: []provider.Model{{ID: "gpt-4o"}},
		},
	}
}

func pinTestServer() *Server {
	provs := pinTestProviders()
	return &Server{
		providerAdder:      &pinOffersAdder{provs: provs},
		providerKeyManager: &pinOffersKeyManager{provs: provs},
	}
}

func TestPrefixedModelRoutingNestedIDs(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		wantPin   bool
		wantOnly  string
		wantModel string
	}{
		{
			// The live-verification bug: a listed nested ID whose
			// first segment is a catalog entry ("liquid") must route
			// verbatim, not pin to the unconfigured catalog provider.
			name:  "listed nested ID is a model, not a pin",
			model: "liquid/lfm-3-1.7b:free", wantPin: false,
			wantModel: "liquid/lfm-3-1.7b:free",
		},
		{
			// Explicit aggregator pin still works: OpenRouter serves
			// the bare nested ID, so the prefix is a real pin.
			name:  "aggregator-prefixed nested ID pins",
			model: "openrouter/liquid/lfm-3-1.7b:free", wantPin: true,
			wantOnly: "openrouter", wantModel: "liquid/lfm-3-1.7b:free",
		},
		{
			name:  "registry-ID pin",
			model: "prov_or/liquid/lfm-3-1.7b:free", wantPin: true,
			wantOnly: "prov_or", wantModel: "liquid/lfm-3-1.7b:free",
		},
		{
			// Both OpenAI and OpenRouter are registered: the explicit
			// provider intent wins over the aggregator's nested ID.
			name:  "direct provider pin wins over aggregator listing",
			model: "openai/gpt-4o", wantPin: true,
			wantOnly: "openai", wantModel: "gpt-4o",
		},
		{
			// Unknown prefix (no catalog entry either): historical
			// behaviour — leave the request untouched for the engine.
			name:  "unknown prefix stays untouched",
			model: "zzvendor/zzmodel", wantPin: false,
			wantModel: "zzvendor/zzmodel",
		},
		{
			name:  "bare model without slash untouched",
			model: "gpt-4o", wantPin: false, wantModel: "gpt-4o",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := pinTestServer()
			req := &provider.ChatRequest{Model: tc.model}
			if err := srv.applyPrefixedModelRouting(req); err != nil {
				t.Fatalf("applyPrefixedModelRouting: %v", err)
			}
			if req.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", req.Model, tc.wantModel)
			}
			if !tc.wantPin {
				if req.Routing != nil {
					t.Errorf("routing = %+v, want nil (no pin)", req.Routing)
				}
				return
			}
			if req.Routing == nil || len(req.Routing.Only) != 1 || req.Routing.Only[0] != tc.wantOnly {
				t.Errorf("routing = %+v, want only [%s]", req.Routing, tc.wantOnly)
			}
		})
	}
}

// TestPrefixedModelRoutingPinConflictsStillError proves the escape
// hatch did not weaken the B2-29 conflict rules: a real pin with a
// conflicting routing contract is still a 400-shaped error.
func TestPrefixedModelRoutingPinConflictsStillError(t *testing.T) {
	srv := pinTestServer()
	req := &provider.ChatRequest{
		Model:   "openrouter/liquid/lfm-3-1.7b:free",
		Routing: &provider.RequestRouting{Group: "g1"},
	}
	if err := srv.applyPrefixedModelRouting(req); err == nil {
		t.Fatal("conflicting routing.group accepted, want error")
	}
}

// TestPrefixedModelRoutingLegacyAdderKeepsPin proves adders without
// the offer surface (test fakes, older wiring) keep the historical
// pin behaviour — the escape hatch is opt-in via the interface.
func TestPrefixedModelRoutingLegacyAdderKeepsPin(t *testing.T) {
	srv := &Server{providerAdder: &loginFakeAdder{}}
	req := &provider.ChatRequest{Model: "liquid/lfm-3-1.7b:free"}
	if err := srv.applyPrefixedModelRouting(req); err != nil {
		t.Fatalf("applyPrefixedModelRouting: %v", err)
	}
	if req.Routing == nil || len(req.Routing.Only) != 1 || req.Routing.Only[0] != "liquid" {
		t.Fatalf("routing = %+v, want legacy pin only [liquid]", req.Routing)
	}
	if req.Model != "lfm-3-1.7b:free" {
		t.Fatalf("model = %q, want bare lfm-3-1.7b:free", req.Model)
	}
}
