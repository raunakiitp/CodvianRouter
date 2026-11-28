// A2-5: qtSd/ virtual-model minting + unwrap.
//
// A "qtSd/<group>/<provider>/<model>" model alias mints a virtual model
// at resolution: the routing group <group> expands to its
// provider/model chain and the request is routed through it as a
// "quota-share" pool (the engine's DRR, orderQuotaShare), then the
// alias is unwrapped to the real targets — the request model becomes
// "<provider>/<model>" and the group's chain is the routing chain.
//
// FAILS-WHEN-OFF: TestQtSdMintResolvesToQuotaSharePool fails if
// resolveQtSdChain is removed — the served model would be the caller's
// chain model, not the alias's concrete "<provider>/<model>", and the
// DRR (which picks the high-share member even though it is listed
// second in the group) would not run.
package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// qtSdEngine builds an engine whose "pool" routing group lists its
// providers as [b, a] (b first) serving the concrete model, with
// quota-share weights that favour a (share 10) over b (share 1). The
// DRR must therefore pick a — the higher share — even though b is
// first in the group's member order; an explicit (non-DRR) order would
// pick b.
func qtSdEngine(t *testing.T) *Engine {
	t.Helper()
	cfg := testConfig()
	cfg.GroupChain = func(group, model string) ([]Attempt, bool) {
		if group != "pool" || model != "prov/m" {
			return nil, false
		}
		return []Attempt{{ProviderID: "b", Model: model}, {ProviderID: "a", Model: model}}, true
	}
	cfg.QuotaShareFor = func(providerID, model string) float64 {
		if providerID == "a" {
			return 10
		}
		if providerID == "b" {
			return 1
		}
		return 1
	}
	return NewEngine(map[string]provider.Client{
		"a": okClient("answer-from-a"),
		"b": okClient("answer-from-b"),
	}, nil, cfg)
}

// TestQtSdMintResolvesToQuotaSharePool: a "qtSd/pool/prov/m" model
// alias resolves to the group's provider/model set and is honoured —
// the request is dialled against the concrete model "prov/m" and the
// quota-share DRR picks provider "a" (share 10) even though "b" is
// listed first in the group's member order. FAILS-WHEN-OFF: without
// the minting, the request routes through the caller's chain at model
// "m" and no DRR runs.
func TestQtSdMintResolvesToQuotaSharePool(t *testing.T) {
	eng := qtSdEngine(t)
	req := comboReq()
	req.Model = "qtSd/pool/prov/m"
	resp, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), req, "")
	if err != nil {
		t.Fatalf("ChatWithChainStrategy(qtSd) = %v, want nil", err)
	}
	if resp.Content != "answer-from-a" {
		t.Fatalf("served content = %q, want the DRR winner %q (a, share 10) — not the group's first-listed member b", resp.Content, "answer-from-a")
	}
	// Unwrap proof: the served attempt is at the concrete model
	// "prov/m" from the alias, not the caller's chain model "m", and
	// the provider is in the group's set.
	tr := eng.LastTrace()
	var served *routetrace.AttemptTrace
	if tr != nil {
		for i := range tr.Attempts {
			if tr.Attempts[i].Outcome == "success" {
				served = &tr.Attempts[i]
				break
			}
		}
	}
	if served == nil {
		t.Fatal("trace recorded no successful attempt")
	}
	if served.Model != "prov/m" {
		t.Fatalf("served model = %q, want the concrete model %q from the qtSd alias (not the caller's %q)", served.Model, "prov/m", "m")
	}
	if served.Provider != "a" {
		t.Fatalf("served provider = %q, want the DRR winner %q from the group's set", served.Provider, "a")
	}
}

// TestQtSdUnknownGroupRejected: a qtSd alias naming an unknown group is
// a 400 naming the pool, never a silent fallback to the caller's chain.
func TestQtSdUnknownGroupRejected(t *testing.T) {
	eng := qtSdEngine(t)
	req := comboReq()
	req.Model = "qtSd/nope/prov/m"
	_, _, err := eng.ChatWithChainStrategy(context.Background(), comboChain("a", "b"), req, "")
	if err == nil {
		t.Fatal("ChatWithChainStrategy(unknown qtSd pool) = nil, want a 400")
	}
	var ce *ContractError
	if !errors.As(err, &ce) || ce.Status != 400 || !strings.Contains(ce.Msg, "nope") {
		t.Fatalf("err = %v, want a 400 naming the unknown pool %q", err, "nope")
	}
}

// TestParseQtSdAlias: the parser splits group / provider / concrete
// model, keeping any "/" inside the concrete model.
func TestParseQtSdAlias(t *testing.T) {
	g, p, m, ok := parseQtSdAlias("qtSd/pool/prov/m")
	if !ok || g != "pool" || p != "prov" || m != "m" {
		t.Fatalf("parseQtSdAlias(qtSd/pool/prov/m) = %q,%q,%q,%v, want pool/prov/m,true", g, p, m, ok)
	}
	g, p, m, ok = parseQtSdAlias("qtSd/pool/prov/meta-llama/llama-3")
	if !ok || g != "pool" || p != "prov" || m != "meta-llama/llama-3" {
		t.Fatalf("parseQtSdAlias(model with slashes) = %q,%q,%q,%v, want pool/prov/meta-llama/llama-3,true", g, p, m, ok)
	}
	if _, _, _, ok := parseQtSdAlias("plain-model"); ok {
		t.Fatal("parseQtSdAlias(plain-model) = ok, want false")
	}
	if _, p, _, ok := parseQtSdAlias("qtSd/pool"); !ok || p != "" {
		t.Fatal("parseQtSdAlias(qtSd/pool) = ok with a provider, want ok with empty provider (malformed)")
	}
}
