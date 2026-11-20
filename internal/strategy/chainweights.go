// Chain per-target weights (M3 Phase C): resolution of the
// operator-configured per-target weights for the "weighted" strategy
// (routing.chain_weights) onto an ordering-target list.
//
// Weights are OPERATOR INTENT, not measured signals: they say how
// much of the draw each target should get relative to its peers
// (e.g. 3 vs 1 means the first target wins ~75% of draws), the way
// an operator splits traffic across providers. They are never
// derived from latency, success rate, cost, or any live telemetry —
// those stay in the scoring factors (see WeightedWeights / WeightsFor
// for the 5-factor component weights, a different thing). A weight of
// 0 excludes that target from the draw while any positive weight
// exists (see WeightedDraw); all weights 1 is a uniform draw.
//
// The coordinator wires Resolve into the weighted-draw hook
// (internal/strategy/wiring.go, StrategyWeighted case): resolve the
// chain's weights, then WeightedDraw over the resolved targets, then
// OrderWeightedFallback. Nothing in this file touches the hook.
package strategy

import (
	"errors"
	"fmt"
	"math"
	"sync"
)

// chainWeightsMu guards the process-wide chain-weights table set by
// the server at startup (see SetChainWeights). The weighted-draw
// hook reads it on every weighted draw; a nil table means uniform
// weights (all 1), exactly as before Phase C.
var (
	chainWeightsMu sync.RWMutex
	chainWeights   map[string]map[string]float64
)

// SetChainWeights installs the operator-configured per-target weight
// table for the "weighted" strategy (routing.chain_weights). It is
// called once by the server at startup, before the engine is built;
// a nil map clears the table (uniform draws). The table is deep-copied
// on install, so later config reloads cannot mutate a live draw.
func SetChainWeights(weights map[string]map[string]float64) {
	chainWeightsMu.Lock()
	defer chainWeightsMu.Unlock()
	if weights == nil {
		chainWeights = nil
		return
	}
	cp := make(map[string]map[string]float64, len(weights))
	for chain, targets := range weights {
		inner := make(map[string]float64, len(targets))
		for k, v := range targets {
			inner[k] = v
		}
		cp[chain] = inner
	}
	chainWeights = cp
}

// chainWeightsFor returns the installed per-target weight table (or
// nil when none was set). The caller passes it to Resolve; a nil
// table resolves every target to weight 1.
func chainWeightsFor() map[string]map[string]float64 {
	chainWeightsMu.RLock()
	defer chainWeightsMu.RUnlock()
	return chainWeights
}

// Resolve applies the per-target weights for one chain to targets and
// returns a copy: each target's Weight is set from cfg[chainKey]
// keyed by target Key() ("provider/model"), or 1 when unconfigured.
// A missing cfg map, a missing chain entry, or a missing target entry
// all mean weight 1. Target keys in cfg that match no target in the
// list are ignored, never an error. The input slice and its elements
// are never mutated; a nil/empty input resolves to an empty list.
func Resolve(chainKey string, targets []OrderingTarget, cfg map[string]map[string]float64) []OrderingTarget {
	out := copyTargets(targets)
	weights, ok := cfg[chainKey]
	if !ok {
		weights = nil
	}
	for i := range out {
		if w, ok := weights[out[i].Key()]; ok {
			out[i].Weight = w
			continue
		}
		out[i].Weight = 1
	}
	return out
}

// ValidateWeights mirrors the config-load rules for chain per-target
// weights (see Config.Validate, routing.chain_weights), so the rules
// live in one place for tests and future callers. It rejects, naming
// the chain and target:
//
//   - a weight that is negative, NaN, or infinite (weights must be
//     finite and >= 0);
//   - a chain whose weights are all zero (a draw over all-zero
//     weights is meaningless — omit the chain, or give a target a
//     positive weight; a single zero among positives is fine);
//   - an empty chain key.
//
// Unknown chain keys are allowed (chains are operator-defined), as
// are unknown target keys (Resolve ignores them), and a nil/empty
// map — or a chain with an empty inner map — is valid and means
// all weights 1.
func ValidateWeights(weights map[string]map[string]float64) error {
	for chain, targets := range weights {
		if chain == "" {
			return errors.New("chain_weights has an empty chain key")
		}
		allZero := true
		for target, w := range targets {
			if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 {
				return fmt.Errorf("chain_weights[%q][%q] must be finite and >= 0 (got %v)", chain, target, w)
			}
			if w > 0 {
				allZero = false
			}
		}
		if len(targets) > 0 && allZero {
			return fmt.Errorf("chain_weights[%q] has all-zero weights: a draw over all-zero weights is meaningless (omit the chain or give a target a positive weight)", chain)
		}
	}
	return nil
}
