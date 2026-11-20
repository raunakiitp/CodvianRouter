// Per-request routing contract (CHECKLIST 157): the routing
// controls a client supplies for one request only, via the
// "routing" request-body field (see provider.RequestRouting).
//
// Precedence for one request (highest first): the contract's sort,
// then the "auto:<axis>" alias in the model name, then the chain
// override, then the global strategy. The contract's order/only/
// exclude/max_price/data_collection filters are applied before any
// scoring — a provider the contract excludes is never scored,
// never tried, and is recorded on the trace with its reason.
//
// Validation is fail-closed and honest: an unknown provider is a
// 400 naming it, an empty result set is a 400 (402 when the price
// ceiling emptied it), never a silent fallback to the full chain.
// The applied contract — controls + every exclusion — is recorded
// on the route trace (routetrace.RoutingContract) so the dashboard
// can show exactly what was applied and why a provider was left
// out, from live data.
package router

import (
	"fmt"
	"math"
	"strings"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/provider"
	"github.com/raunakiitp/codvian-router/internal/routetrace"
)

// ContractError is a per-request routing-contract rejection: the
// request itself was refused before any provider was dialled. The
// server maps Status to the HTTP status (400 for a bad control or
// an empty result set, 402 when the price ceiling left nothing)
// and Msg to the client-facing message, which names the offending
// control and provider — never an upstream detail.
type ContractError struct {
	Status int
	Msg    string
}

func (e *ContractError) Error() string { return e.Msg }

func contractErr(status int, format string, args ...any) *ContractError {
	return &ContractError{Status: status, Msg: fmt.Sprintf(format, args...)}
}

// sortStrategy maps a routing.sort value to the strategy name the
// engine orders by: price -> cheapest, latency -> least-latency
// (pure latency, the literal reading of "sort by latency"),
// reliability -> most-reliable. "" means "no sort requested".
func sortStrategy(sort string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(sort)) {
	case "":
		return "", true
	case "price":
		return "cheapest", true
	case "latency":
		return "least-latency", true
	case "reliability":
		return "most-reliable", true
	default:
		return "", false
	}
}

// applyContract applies req.Routing to chain and returns the
// filtered chain, the trace contract (nil when the request carries
// no non-empty contract), the strategy the contract's sort maps
// to ("" when no sort was requested), and an error when the
// contract is invalid or leaves no provider. The returned
// contract is non-nil whenever a non-empty contract was supplied,
// even on error — its Error field then carries the rejection, so
// the caller can publish the trace and the dashboard can show
// why the request was refused.
//
// The filters run in a fixed order (only, exclude,
// data_collection, max_price, order) and the empty-set error is
// raised by the stage that emptied the set, so a 402 names the
// price ceiling only when the ceiling is what emptied it.
func (e *Engine) applyContract(chain []Attempt, req provider.ChatRequest) (filtered []Attempt, contract *routetrace.RoutingContract, sortStrat string, err error) {
	rc := req.Routing
	if rc.Empty() {
		return chain, nil, "", nil
	}
	contract = &routetrace.RoutingContract{
		Order:          append([]string(nil), rc.Order...),
		Only:           append([]string(nil), rc.Only...),
		Exclude:        append([]string(nil), rc.Exclude...),
		Sort:           rc.Sort,
		DataCollection: rc.DataCollection,
		Group:          strings.TrimSpace(rc.Group),
	}
	if rc.MaxPrice != nil {
		contract.MaxPrice = &routetrace.RoutingMaxPrice{Input: rc.MaxPrice.Input, Output: rc.MaxPrice.Output}
	}
	fail := func(status int, format string, args ...any) ([]Attempt, *routetrace.RoutingContract, string, error) {
		ce := contractErr(status, format, args...)
		contract.Error = ce.Msg
		return nil, contract, "", ce
	}

	// --- value validation (before any filtering) ---
	sortStrat, ok := sortStrategy(rc.Sort)
	if !ok {
		return fail(400, "routing.sort: unknown sort %q (want price, latency or reliability)", rc.Sort)
	}
	dc := strings.ToLower(strings.TrimSpace(rc.DataCollection))
	if dc != "" && dc != "deny" && dc != "allow" {
		return fail(400, "routing.data_collection: unknown value %q (want deny or allow)", rc.DataCollection)
	}
	if rc.MaxPrice != nil {
		if rc.MaxPrice.Input == nil && rc.MaxPrice.Output == nil {
			return fail(400, "routing.max_price: set at least one of input or output (USD per 1M tokens)")
		}
		for name, v := range map[string]*float64{"input": rc.MaxPrice.Input, "output": rc.MaxPrice.Output} {
			if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
				return fail(400, "routing.max_price.%s: price ceiling must be a non-negative number, got %v", name, *v)
			}
		}
	}
	if len(rc.Order) > 0 && rc.Sort != "" {
		return fail(400, "routing.order and routing.sort conflict: order already fixes the try order")
	}
	// Normalise names once: trim surrounding space, drop empties
	// (an empty name is a 400 naming the field, not a silent skip).
	norm := func(field string, names []string) ([]string, error) {
		out := make([]string, 0, len(names))
		seen := map[string]bool{}
		for _, n := range names {
			n = strings.TrimSpace(n)
			if n == "" {
				return nil, fmt.Errorf("routing.%s: provider names must not be empty", field)
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
		return out, nil
	}
	order, err := norm("order", rc.Order)
	if err != nil {
		return fail(400, "%s", err.Error())
	}
	only, err := norm("only", rc.Only)
	if err != nil {
		return fail(400, "%s", err.Error())
	}
	exclude, err := norm("exclude", rc.Exclude)
	if err != nil {
		return fail(400, "%s", err.Error())
	}
	// Resolve names to registry IDs: clients address providers
	// by the display names they see, while chains are keyed by
	// ID. The trace records the resolved IDs, so its provider
	// fields line up with the attempt rows. With no resolver
	// wired, names are already IDs and resolve to themselves.
	if e.cfg.ResolveProvider != nil {
		resolve := func(field string, names []string) ([]string, error) {
			out := make([]string, 0, len(names))
			for _, n := range names {
				id, rerr := e.cfg.ResolveProvider(n)
				if rerr != nil {
					return nil, fmt.Errorf("routing.%s: %s", field, rerr.Error())
				}
				out = append(out, id)
			}
			return out, nil
		}
		if order, err = resolve("order", order); err != nil {
			return fail(400, "%s", err.Error())
		}
		if only, err = resolve("only", only); err != nil {
			return fail(400, "%s", err.Error())
		}
		if exclude, err = resolve("exclude", exclude); err != nil {
			return fail(400, "%s", err.Error())
		}
		// The contract echo carries the resolved IDs, so the
		// dashboard's exclusion rows and attempt rows name the
		// same providers.
		contract.Order = append([]string(nil), order...)
		contract.Only = append([]string(nil), only...)
		contract.Exclude = append([]string(nil), exclude...)
	}
	// Unknown providers: every name must be a registered client.
	// A name the router has never seen is a 400 naming it —
	// never a silent drop, which would hide a typo'd provider.
	for field, names := range map[string][]string{"order": order, "only": only, "exclude": exclude} {
		for _, n := range names {
			if !e.hasClient(n) {
				return fail(400, "routing.%s: unknown provider %q", field, n)
			}
		}
	}
	// only ∩ exclude is a contradiction, not a filter.
	onlySet := map[string]bool{}
	for _, n := range only {
		onlySet[n] = true
	}
	for _, n := range exclude {
		if onlySet[n] {
			return fail(400, "routing.only and routing.exclude both list provider %q", n)
		}
	}
	// order must only name providers that serve this model: an
	// order entry outside the chain could never be tried, so
	// listing it would silently change the request's meaning.
	chainSet := map[string]bool{}
	for _, att := range chain {
		chainSet[att.ProviderID] = true
	}
	for _, n := range order {
		if !chainSet[n] {
			return fail(400, "routing.order: provider %q does not serve model %q", n, req.Model)
		}
	}

	excluded := func(id, reason string) {
		contract.Excluded = append(contract.Excluded, routetrace.RoutingExclusion{Provider: id, Reason: reason})
	}

	// --- filtering, in fixed order ---
	filtered = append([]Attempt(nil), chain...)
	// only: keep only the listed providers.
	if len(only) > 0 {
		var kept []Attempt
		for _, att := range filtered {
			if onlySet[att.ProviderID] {
				kept = append(kept, att)
			} else {
				excluded(att.ProviderID, "not_in_only")
			}
		}
		filtered = kept
		if len(filtered) == 0 {
			return fail(400, "routing.only: no listed provider serves model %q", req.Model)
		}
	}
	// exclude: drop the listed providers.
	if len(exclude) > 0 {
		exSet := map[string]bool{}
		for _, n := range exclude {
			exSet[n] = true
		}
		var kept []Attempt
		for _, att := range filtered {
			if exSet[att.ProviderID] {
				excluded(att.ProviderID, "excluded")
			} else {
				kept = append(kept, att)
			}
		}
		filtered = kept
		if len(filtered) == 0 {
			return fail(400, "routing.exclude: every provider for model %q is excluded", req.Model)
		}
	}
	// data_collection=deny: keep only providers verified as not
	// collecting training data. A nil TrainingSafe func means the
	// router cannot verify any provider, so none is kept — the
	// control fails closed, never silently allowed.
	if dc == "deny" {
		var kept []Attempt
		for _, att := range filtered {
			if e.cfg.TrainingSafe != nil && e.cfg.TrainingSafe(att.ProviderID) {
				kept = append(kept, att)
			} else {
				excluded(att.ProviderID, "data_collection_deny")
			}
		}
		filtered = kept
		if len(filtered) == 0 {
			return fail(400, "routing.data_collection=deny: no provider for model %q is verified as excluding training-data collection (mark providers with no_training_data in config, or use a local provider)", req.Model)
		}
	}
	// max_price: keep only pairs whose known price fits every
	// ceiling leg set. A pair with no operator-configured price
	// is excluded as price_unknown — the ceiling cannot be
	// verified against a price nobody configured, so the pair is
	// never assumed cheap.
	if rc.MaxPrice != nil {
		var kept []Attempt
		over, unknown := 0, 0
		for _, att := range filtered {
			price, ok := e.priceFor(att.ProviderID, att.Model)
			switch {
			case !ok:
				unknown++
				excluded(att.ProviderID, "price_unknown")
			case priceVerdict(rc.MaxPrice, price) == "over":
				over++
				excluded(att.ProviderID, "over_max_price")
			case priceVerdict(rc.MaxPrice, price) == "unknown":
				// Half-known price: a ceiling leg is set but
				// that leg has no configured price, so the
				// ceiling cannot be verified for this pair.
				unknown++
				excluded(att.ProviderID, "price_unknown")
			default:
				kept = append(kept, att)
			}
		}
		filtered = kept
		if len(filtered) == 0 {
			return fail(402, "routing.max_price: no provider for model %q fits the price ceiling (%s); %d excluded: price above ceiling, %d excluded: price unknown", req.Model, maxPriceDesc(rc.MaxPrice), over, unknown)
		}
	}
	// order: reorder to the listed order; chain providers not
	// listed are excluded, never silently appended.
	if len(order) > 0 {
		pos := map[string]int{}
		for i, n := range order {
			pos[n] = i
		}
		ordered := make([]Attempt, 0, len(filtered))
		seen := map[string]bool{}
		for _, n := range order {
			for _, att := range filtered {
				if att.ProviderID == n && !seen[att.ProviderID] {
					ordered = append(ordered, att)
					seen[att.ProviderID] = true
				}
			}
		}
		for _, att := range filtered {
			if !seen[att.ProviderID] {
				excluded(att.ProviderID, "not_in_order")
			}
		}
		filtered = ordered
	}
	return filtered, contract, sortStrat, nil
}

// priceFor returns the operator-configured price for a
// (provider, model) pair via Config.ContractPriceFunc: pair price first,
// then the model-level fallback — the same precedence the price
// book's Lookup applies. A nil func means no price is known for
// any pair, so every max_price check excludes as price_unknown.
func (e *Engine) priceFor(providerID, model string) (config.ModelPrice, bool) {
	if e.cfg.ContractPriceFunc == nil {
		return config.ModelPrice{}, false
	}
	return e.cfg.ContractPriceFunc(providerID, model)
}

// priceVerdict checks one pair's price against the ceiling, leg
// by leg: "fit" when every ceiling leg set has a known price at or
// under it, "over" when a known leg exceeds its ceiling, "unknown"
// when a ceiling leg is set but that leg has no configured price
// (the ceiling cannot be verified for this pair).
func priceVerdict(mp *provider.MaxPrice, price config.ModelPrice) string {
	if mp.Input != nil {
		if price.InputUSDPer1M == nil {
			return "unknown"
		}
		if *price.InputUSDPer1M > *mp.Input {
			return "over"
		}
	}
	if mp.Output != nil {
		if price.OutputUSDPer1M == nil {
			return "unknown"
		}
		if *price.OutputUSDPer1M > *mp.Output {
			return "over"
		}
	}
	return "fit"
}

// maxPriceDesc renders the ceiling for the 402 message.
func maxPriceDesc(mp *provider.MaxPrice) string {
	var parts []string
	if mp.Input != nil {
		parts = append(parts, fmt.Sprintf("input ≤ $%g/1M tokens", *mp.Input))
	}
	if mp.Output != nil {
		parts = append(parts, fmt.Sprintf("output ≤ $%g/1M tokens", *mp.Output))
	}
	return strings.Join(parts, ", ")
}
