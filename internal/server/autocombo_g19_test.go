// G19 Area 1: premium + vision narrowings and removed-provider
// exclusion for the candidate-pool endpoint.
package server

import (
	"net/http"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/autorouter"
	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
)

// TestG19AutoComboProFastNarrowsToPremium proves the "pro"
// TierFilter is applied from the real price book: pro-fast lists
// only the premium-tier models. The blended leg decides here
// (no key manager wired): gpt-5 ($6.25/M), claude-opus ($10/M)
// and deepseek-chat ($1.50/M) are premium; glm-free ($0) is
// free and excluded. Removing the narrowing fails this test
// (the whole 4-model union would be listed).
func TestG19AutoComboProFastNarrowsToPremium(t *testing.T) {
	srv, key := newAutoComboTestServerWith(t, WithPriceBook(autoComboTestPriceBook()))
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/pro-fast/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pro-fast candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 3 {
		t.Fatalf("candidates = %d rows %+v, want 3 (premium only: glm-free excluded)", len(res.Candidates), res.Candidates)
	}
	for _, model := range []string{"openai/gpt-5", "cc/claude-opus", "deepseek/deepseek-chat"} {
		findAutoComboRow(t, res, model)
	}
}

// TestG19AutoComboProFastFailOpenWithoutPrices proves the
// premium filter is fail-open exactly like free/reasoning: with
// no price book no model can be proven premium, so the whole
// union is listed.
func TestG19AutoComboProFastFailOpenWithoutPrices(t *testing.T) {
	srv, key := newAutoComboTestServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/pro-fast/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET pro-fast candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 4 {
		t.Fatalf("candidates = %d rows %+v, want 4 (whole tier union, fail-open: no price data)", len(res.Candidates), res.Candidates)
	}
}

// TestG19AutoComboNarrowPremiumInputLeg proves the input leg
// decides the tier when the registry model carries one:
// deepseek-chat's $1/M input is cheap even though its blended
// $1.50/M would be premium, and a model with no price anywhere
// is unclassified, never premium.
func TestG19AutoComboNarrowPremiumInputLeg(t *testing.T) {
	srv := &Server{
		priceBook: autoComboTestPriceBook(),
		providerKeyManager: &autoComboFakeKeyManager{providers: map[string]*provider.Provider{
			"openai":   {ID: "openai", Models: []provider.Model{{ID: "gpt-5", InputPriceUSD: 2.5e-6}}},
			"deepseek": {ID: "deepseek", Models: []provider.Model{{ID: "deepseek-chat", InputPriceUSD: 1e-6}}},
			"cc":       {ID: "cc", Models: []provider.Model{{ID: "claude-opus"}}},
		}},
	}
	got := srv.autoComboNarrowPremium([]string{"openai/gpt-5", "deepseek/deepseek-chat", "cc/claude-opus", "unknown/no-price-model"})
	want := []string{"openai/gpt-5", "cc/claude-opus"}
	if len(got) != len(want) {
		t.Fatalf("narrowed = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("narrowed = %v, want %v", got, want)
		}
	}
}

// TestG19AutoComboNarrowVision proves the vision filter reads
// the registry's synced vision flag: only a model the registry
// reports as vision-capable survives. A model the registry does
// not know, or reports without vision, is not vision-capable —
// never guessed from the model id.
func TestG19AutoComboNarrowVision(t *testing.T) {
	srv := &Server{providerKeyManager: &autoComboFakeKeyManager{providers: map[string]*provider.Provider{
		"openai":   {ID: "openai", Models: []provider.Model{{ID: "gpt-5", Vision: true}}},
		"deepseek": {ID: "deepseek", Models: []provider.Model{{ID: "deepseek-chat"}}},
	}}}
	got := srv.autoComboNarrowVision([]string{"openai/gpt-5", "deepseek/deepseek-chat", "cc/claude-opus"})
	if len(got) != 1 || got[0] != "openai/gpt-5" {
		t.Fatalf("narrowed = %v, want [openai/gpt-5]", got)
	}
	if got := (&Server{}).autoComboNarrowVision([]string{"openai/gpt-5"}); len(got) != 0 {
		t.Fatalf("narrowed with no key manager = %v, want [] (no vision source, fail-open is the caller's job)", got)
	}
}

// newG19AutoComboRemovedServer builds a server whose chat tier
// holds one removed provider (puter) and one ordinary provider
// (openai, vision-capable): the pool must never list the removed
// provider, and the vision narrowing must list only the
// vision-capable model.
func newG19AutoComboRemovedServer(t *testing.T) (*Server, string) {
	t.Helper()
	ar := autorouter.New(autorouter.Config{TierModels: map[autorouter.Tier][]string{
		autorouter.TierChat: {"puter/puter-model", "openai/gpt-5"},
	}})
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{ID: "puter", Name: "puter", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-p"}}},
		{ID: "openai", Name: "openai", Enabled: true, Keys: []provider.ProviderKey{{ID: "conn-b"}}},
	}}
	k, raw, err := config.IssueKey("g19-autocombo-test", []string{"models"}, 0)
	if err != nil {
		t.Fatalf("config.IssueKey: %v", err)
	}
	if err := cfg.AddGatewayKey(k); err != nil {
		t.Fatalf("cfg.AddGatewayKey: %v", err)
	}
	srv := newTestServer(t, &autoComboTestEngine{}, WithAutoRouter(ar), WithConfig(cfg))
	// A removed provider can never be loaded into a real
	// registry (the load path refuses it), so the fake key
	// manager stands in for a registry that gained the entry
	// another way — the pool filter is defense-in-depth for
	// exactly that case.
	srv.providerKeyManager = &autoComboFakeKeyManager{providers: map[string]*provider.Provider{
		"puter":  {ID: "puter", CatalogID: "puter", Keys: []provider.ProviderKey{{ID: "conn-p"}}, Models: []provider.Model{{ID: "puter-model"}}},
		"openai": {ID: "openai", Keys: []provider.ProviderKey{{ID: "conn-b"}}, Models: []provider.Model{{ID: "gpt-5", Vision: true}}},
	}}
	return srv, raw
}

// TestG19AutoComboRemovedProviderExcluded proves a removed
// provider never appears in the candidate pool, even when the
// tier lists its model.
func TestG19AutoComboRemovedProviderExcluded(t *testing.T) {
	srv, key := newG19AutoComboRemovedServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/chat/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET chat candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d rows %+v, want 1 (only openai/gpt-5; puter excluded)", len(res.Candidates), res.Candidates)
	}
	findAutoComboRow(t, res, "openai/gpt-5")
}

// TestG19AutoComboBestVisionNarrowsByRegistry proves the vision
// category filter is applied from the registry's synced flag:
// best-vision lists only the vision-capable model.
func TestG19AutoComboBestVisionNarrowsByRegistry(t *testing.T) {
	srv, key := newG19AutoComboRemovedServer(t)
	rec := doReq(t, srv, http.MethodGet, "/v1/auto-combo/best-vision/candidates", "", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET best-vision candidates: status %d, want 200: %s", rec.Code, rec.Body.String())
	}
	res := decodeAutoComboCandidates(t, rec.Body.Bytes())
	if len(res.Candidates) != 1 {
		t.Fatalf("candidates = %d rows %+v, want 1 (only openai/gpt-5 is vision-capable)", len(res.Candidates), res.Candidates)
	}
	findAutoComboRow(t, res, "openai/gpt-5")
}

// TestG19AutoComboProviderRemovedFallback proves the config
// fallback of the removed check: a whole-integration removal
// matches by id, a keyless-only removal matches only a keyless
// entry, and an ordinary provider never matches.
func TestG19AutoComboProviderRemovedFallback(t *testing.T) {
	keyed := &Server{cfg: &config.Config{Providers: []config.ProviderConfig{
		{ID: "puter", Keys: []provider.ProviderKey{{ID: "conn-p"}}},
		{ID: "opencode", Keys: []provider.ProviderKey{{ID: "conn-o"}}},
		{ID: "openai", Keys: []provider.ProviderKey{{ID: "conn-b"}}},
	}}}
	if !keyed.autoComboProviderRemoved("puter") {
		t.Fatal("puter (config, with keys) not reported removed")
	}
	if keyed.autoComboProviderRemoved("opencode") {
		t.Fatal("opencode (config, with keys) reported removed: only the keyless surface was removed")
	}
	if keyed.autoComboProviderRemoved("openai") {
		t.Fatal("openai reported removed")
	}
	if keyed.autoComboProviderRemoved("no-such-provider") {
		t.Fatal("unknown provider reported removed")
	}
	keyless := &Server{cfg: &config.Config{Providers: []config.ProviderConfig{
		{ID: "opencode"},
	}}}
	if !keyless.autoComboProviderRemoved("opencode") {
		t.Fatal("opencode (config, keyless) not reported removed")
	}
}
