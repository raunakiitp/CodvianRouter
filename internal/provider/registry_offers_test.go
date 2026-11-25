package provider

// Offer queries (provider-quirks audit slice): the server uses these
// to tell a full nested model ID apart from a "provider/model"
// routing pin. They mirror the chain builder's offer rule — an
// enabled provider with an empty model list serves any model.

import "testing"

func TestProviderOffersModelLocked(t *testing.T) {
	listed := &Provider{Enabled: true, Models: []Model{{ID: "vendor/model"}, {ID: "bare"}}}
	if !providerOffersModelLocked(listed, "vendor/model") {
		t.Error("listed nested ID not offered")
	}
	if !providerOffersModelLocked(listed, "bare") {
		t.Error("listed bare ID not offered")
	}
	if providerOffersModelLocked(listed, "vendor/other") {
		t.Error("unlisted ID offered")
	}
	empty := &Provider{Enabled: true}
	if !providerOffersModelLocked(empty, "anything/at-all") {
		t.Error("empty model list must serve any model (chain builder rule)")
	}
}

func TestRegistryOffersModel(t *testing.T) {
	srv := openAIMock(t)
	defer srv.Close()

	reg := NewRegistry(nil, WithRegistryAllowPrivateDestinations(true))
	p, err := reg.Add(srv.URL, "sk-test")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Discovered models are offered verbatim; unknown IDs are not.
	if !reg.OffersModel("model-a") || !reg.OffersModel("model-b") {
		t.Error("discovered models not offered")
	}
	if reg.OffersModel("no-such-model") {
		t.Error("unknown model offered by a listed provider")
	}

	// Provider lookup by registry ID and by display name.
	if !reg.ProviderOffersModel(p.ID, "model-a") {
		t.Error("offer by registry ID failed")
	}
	if !reg.ProviderOffersModel(p.Name, "model-a") {
		t.Error("offer by display name failed")
	}
	if reg.ProviderOffersModel("no-such-provider", "model-a") {
		t.Error("unknown provider offered a model")
	}
	if reg.ProviderOffersModel(p.ID, "no-such-model") {
		t.Error("provider offered an unlisted model")
	}

	// Disabled providers offer nothing.
	if err := reg.SetEnabled(p.ID, false); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if reg.OffersModel("model-a") {
		t.Error("disabled provider still offers models")
	}
	if reg.ProviderOffersModel(p.ID, "model-a") {
		t.Error("disabled provider still offers via ProviderOffersModel")
	}
}
