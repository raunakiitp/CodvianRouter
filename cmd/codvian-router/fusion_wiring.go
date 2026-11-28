package main

import (
	"strings"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// applyFusionRouting wires the fusion judge + panel knobs from the
// config file into the engine configuration (A2-3b).
//
// Why this exists: the engine grew the fields (router.Config.JudgeModel,
// ComboMinPanel, ComboMaxPanel, ComboPanelHardTimeoutMs,
// ComboStragglerGraceMs) but NO production code ever set them - the only
// production builder of router.Config is openStack in main.go. The fusion
// LLM judge and the panel knobs were therefore unreachable from a real
// request: a config-file value could not change them and the engine fell
// back to its own defaults.
//
// The judge model is optional: an empty value means "the first panel
// member" (the fusion default), which the engine resolves itself - so an
// operator pins a judge model only to choose one explicitly.
//
// Every value here was validated at config load (negative panel sizes or
// timeouts are a load error), so 0 means "keep the engine default" and a
// positive value is the operator's number.
func applyFusionRouting(engCfg *router.Config, routing *config.RoutingConfig) {
	if engCfg == nil || routing == nil {
		return
	}
	if jm := strings.TrimSpace(routing.FusionJudgeModel); jm != "" {
		engCfg.JudgeModel = func(string) string { return jm }
	}
	if routing.FusionMinPanel > 0 {
		engCfg.ComboMinPanel = routing.FusionMinPanel
	}
	if routing.FusionMaxPanel > 0 {
		engCfg.ComboMaxPanel = routing.FusionMaxPanel
	}
	if routing.FusionStragglerGraceMs > 0 {
		engCfg.ComboStragglerGraceMs = int(routing.FusionStragglerGraceMs)
	}
	if routing.FusionPanelHardTimeoutMs > 0 {
		engCfg.ComboPanelHardTimeoutMs = int(routing.FusionPanelHardTimeoutMs)
	}
}
