// qtSd/ virtual-model minting (A2-5): a caller can address a
// quota-share combo directly as a single model alias —
//
//	qtSd/<group>/<provider>/<model>
//
// The alias mints a virtual model at resolution: the routing group
// <group> is expanded to its provider/model chain and the request is
// routed through it as a "quota-share" pool — the engine's own DRR
// (orderQuotaShare, cascade.go) orders the members by their configured
// share (Config.QuotaShareFor), so the combo is honoured exactly as if
// the caller had set strategy=quota-share over that chain. The alias
// is then unwrapped to the real targets: the request's model is
// rewritten to <provider>/<model> and the group's chain becomes the
// routing chain, so the request flows through the engine exactly like
// any other chain (the same failover, budget gate, ledger rows and
// contract filters), only the execution order is DRR.
//
// <group> reuses the operator-configured routing groups (routing.groups,
// via Config.GroupChain) — the named pool of providers. <provider>/<model>
// is the concrete model routed through the pool; a model ID may itself
// contain "/", so only the first two slashes after "qtSd/" split. The
// quota-share weights come from the same table the "quota-share"
// strategy reads (Config.QuotaShareFor, routing.quota_shares); a group
// with no shares table shares evenly. An unknown group fails honestly
// with a 400 naming it; a group with no member serving the model is a
// 400 naming the group and the model. A non-qtSd model is untouched.
package router

import (
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
)

// qtSdPrefix is the case-insensitive prefix of a qtSd/ virtual-model
// alias.
const qtSdPrefix = "qtSd/"

// parseQtSdAlias parses a "qtSd/<group>/<provider>/<model>" model
// alias into its group name, provider and concrete model. The first
// two slashes after the prefix split the three parts; a model ID may
// itself contain "/" (e.g. "meta-llama/llama-3"), which stays in the
// concrete model. It returns ok=false when the model is not a qtSd
// alias at all; a malformed alias (missing parts, empty part) is still
// ok=true with the offending part empty, so the caller can fail it
// honestly naming the alias instead of treating it as a plain model.
func parseQtSdAlias(model string) (group, provider, concrete string, ok bool) {
	if len(model) <= len(qtSdPrefix) || !strings.EqualFold(model[:len(qtSdPrefix)], qtSdPrefix) {
		return "", "", "", false
	}
	rest := strings.TrimSpace(model[len(qtSdPrefix):])
	first := strings.Index(rest, "/")
	if first < 0 {
		return "", "", "", true
	}
	second := strings.Index(rest[first+1:], "/")
	if second < 0 {
		return strings.TrimSpace(rest[:first]), "", "", true
	}
	providerPart := strings.TrimSpace(rest[first+1 : first+1+second])
	concretePart := strings.TrimSpace(rest[first+1+second+1:])
	return strings.TrimSpace(rest[:first]), providerPart, concretePart, true
}

// resolveQtSdChain resolves a qtSd/-addressed request to the chain it
// routes through, minting the virtual model and unwrapping it to the
// real targets. It returns the (possibly rewritten) request — a
// "qtSd/..." alias has its model replaced by the concrete
// "<provider>/<model>" — the chain to use, the qtSd group name ("" when
// the request named no qtSd alias), whether the alias was minted, and
// an error when the addressing is bad or the group cannot serve it.
// The error is always a *ContractError with status 400, so the server
// answers it like any other per-request contract rejection.
//
// The caller (chatWithChainStrategy / runStream) forces the effective
// strategy to "quota-share" when minted, so orderQuotaShare (the
// engine's DRR) orders the unwrapped chain. Unlike a plain routing
// group (resolveGroupChain), a qtSd pool is NOT an explicit member
// order: the DRR picks the winner, so the chain is not marked
// explicit and the hedge is not disabled.
func (e *Engine) resolveQtSdChain(chain []Attempt, req provider.ChatRequest) (provider.ChatRequest, []Attempt, string, bool, *ContractError) {
	aliasGroup, aliasProvider, aliasModel, isAlias := parseQtSdAlias(strings.TrimSpace(req.Model))
	if !isAlias {
		return req, chain, "", false, nil
	}
	if aliasGroup == "" {
		return req, nil, "", false, contractErr(400, "model %q names a quota-share pool but no group: use qtSd/<group>/<provider>/<model>", req.Model)
	}
	if aliasProvider == "" {
		return req, nil, "", false, contractErr(400, "model %q names quota-share pool %q but no provider: use qtSd/<group>/<provider>/<model>", req.Model, aliasGroup)
	}
	if aliasModel == "" {
		return req, nil, "", false, contractErr(400, "model %q names quota-share pool %q but no model: use qtSd/<group>/<provider>/<model>", req.Model, aliasGroup)
	}
	if e.cfg.GroupChain == nil {
		return req, nil, "", false, contractErr(400, "unknown quota-share pool %q: no routing groups are configured", aliasGroup)
	}
	concrete := aliasProvider + "/" + aliasModel
	gchain, exists := e.cfg.GroupChain(aliasGroup, concrete)
	if !exists {
		return req, nil, "", false, contractErr(400, "unknown quota-share pool %q", aliasGroup)
	}
	if len(gchain) == 0 {
		return req, nil, "", false, contractErr(400, "quota-share pool %q has no provider serving model %q", aliasGroup, concrete)
	}
	req.Model = concrete
	return req, gchain, aliasGroup, true, nil
}
