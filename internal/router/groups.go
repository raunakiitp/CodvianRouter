// User-named routing groups (CHECKLIST 173): an operator names a
// pool of providers once in the config file (routing.groups) and
// addresses it as one routing target — by name in a request
// (routing.group, or the "group:<name>/<model>" model alias) or as
// a "group:<name>" entry in a refusal chain. A group resolves to
// its members in order and then flows through the engine exactly
// like any other chain: the same failover, the same per-request
// budget gate, the same ledger rows, the same contract filters,
// the same context-fit check. An unknown group name fails
// honestly with a 400 naming it — never a silent fallback to
// everything — and a group with no member serving the model fails
// with a 400 naming the group and the model.
//
// Resolution lives in ChatWithChainStrategy /
// ChatStreamWithChainStrategy (not in the server-facing Chat /
// ChatStream): the group replaces the caller-supplied chain at
// the top of both, before the per-request contract is applied,
// so a group-addressed request is group-routed on every path and
// a direct chain caller that names a group gets the group chain,
// never a silently mixed one. A request that names no group is
// untouched: its chain passes through unchanged.
package router

import (
	"strings"

	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// parseGroupAlias parses a "group:<name>/<model>" model alias: the
// group name before the first "/" and the concrete model after it
// (a model ID may itself contain "/", e.g. "meta-llama/llama-3",
// so only the first slash splits). It returns ok=false when the
// model is not a group alias at all; a malformed alias (no slash,
// empty name, empty model) is still ok=true with the offending
// part empty, so the caller can fail it honestly naming the
// alias instead of treating it as an ordinary model name.
func parseGroupAlias(model string) (group, concrete string, ok bool) {
	if len(model) <= 6 || !strings.EqualFold(model[:6], "group:") {
		return "", "", false
	}
	rest := strings.TrimSpace(model[len("group:"):])
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", true
	}
	return strings.TrimSpace(rest[:slash]), strings.TrimSpace(rest[slash+1:]), true
}

// resolveGroupChain resolves a group-addressed request to the chain
// it routes through. It returns the (possibly rewritten) request —
// a "group:<name>/<model>" alias has its model replaced by the
// concrete model — the chain to use, the group name ("" when the
// request named no group), and an error when the addressing is bad
// or the group cannot serve this request. The error is always a
// *ContractError with status 400, so the server answers it like
// any other per-request contract rejection (see
// handleEngineError): the request was refused before any provider
// was dialled.
//
// Addressing, in order: routing.group wins when set; a
// "group:<name>/<model>" model alias names the group and the
// model together. When both spellings are present they must name
// the same group — a conflict is a 400 naming both. A group the
// engine has no chain for (GroupChain nil, or exists=false) is an
// unknown group: a 400 naming it. An existing group that expands
// to no attempt for the model is a 400 naming the group and the
// model — the members may all be disabled, clientless, or not
// serving this model, and silently falling back to the full chain
// would route somewhere the operator never named.
func (e *Engine) resolveGroupChain(chain []Attempt, req provider.ChatRequest) (provider.ChatRequest, []Attempt, string, *ContractError) {
	group := ""
	if req.Routing != nil {
		group = strings.TrimSpace(req.Routing.Group)
	}
	if aliasGroup, aliasModel, isAlias := parseGroupAlias(strings.TrimSpace(req.Model)); isAlias {
		if aliasGroup == "" && aliasModel == "" {
			// No slash at all: "group:fast" names a group but
			// no model — there is nothing to route to.
			return req, nil, "", contractErr(400, "model %q names a routing group but no model: use group:<name>/<model> (for example group:fast/gpt-4o)", req.Model)
		}
		if aliasGroup == "" {
			return req, nil, "", contractErr(400, "model %q names a routing group but the group name is empty: use group:<name>/<model>", req.Model)
		}
		if aliasModel == "" {
			return req, nil, "", contractErr(400, "model %q names routing group %q but no model: use group:<name>/<model>", req.Model, aliasGroup)
		}
		if group != "" && group != aliasGroup {
			return req, nil, "", contractErr(400, "routing.group %q conflicts with model alias group %q: name the group once", group, aliasGroup)
		}
		group = aliasGroup
		req.Model = aliasModel
	}
	if group == "" {
		return req, chain, "", nil
	}
	if e.cfg.GroupChain == nil {
		return req, nil, group, contractErr(400, "unknown routing group %q: no groups are configured", group)
	}
	gchain, exists := e.cfg.GroupChain(group, req.Model)
	if !exists {
		return req, nil, group, contractErr(400, "unknown routing group %q", group)
	}
	if len(gchain) == 0 {
		return req, nil, group, contractErr(400, "routing group %q has no provider serving model %q", group, req.Model)
	}
	return req, gchain, group, nil
}

// groupFailureTrace builds the trace a rejected group-addressed
// request publishes: the contract block carrying the group and
// the rejection, with no attempts — so the dashboard shows why
// the request was refused instead of a stale trace from an
// earlier request. It mirrors the contract-rejection publish in
// ChatWithChainStrategy / ChatStreamWithChainStrategy.
func groupFailureTrace(group string, err error) *routetrace.RouteTrace {
	return &routetrace.RouteTrace{Contract: &routetrace.RoutingContract{Group: group, Error: err.Error()}}
}
