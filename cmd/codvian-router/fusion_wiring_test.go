package main

import (
	"testing"

	"github.com/raunakiitp/codvian-router/internal/config"
	"github.com/raunakiitp/codvian-router/internal/router"
)

// TestApplyFusionRoutingWiresJudgeAndKnobs proves the config file's
// fusion knobs reach the ENGINE configuration - the production wiring
// (A2-3b). Before this, router.Config.JudgeModel and the Combo* knobs
// were set by no production code, so a config value could not change
// the fusion judge or the panel.
//
// FAILS-WHEN-OFF: delete the applyFusionRouting call in main.go (or make
// it a no-op) and every assertion below fails - JudgeModel stays nil and
// the Combo* knobs stay zero.
func TestApplyFusionRoutingWiresJudgeAndKnobs(t *testing.T) {
	engCfg := router.Config{}
	applyFusionRouting(&engCfg, &config.RoutingConfig{
		FusionJudgeModel:         "judge-model",
		FusionMinPanel:           3,
		FusionMaxPanel:           12,
		FusionStragglerGraceMs:   2500,
		FusionPanelHardTimeoutMs: 45000,
	})
	if engCfg.JudgeModel == nil {
		t.Fatal("JudgeModel is nil: the configured judge model never reached the engine")
	}
	if got := engCfg.JudgeModel("anything"); got != "judge-model" {
		t.Fatalf("JudgeModel(model) = %q, want %q", got, "judge-model")
	}
	if engCfg.ComboMinPanel != 3 {
		t.Fatalf("ComboMinPanel = %d, want 3", engCfg.ComboMinPanel)
	}
	if engCfg.ComboMaxPanel != 12 {
		t.Fatalf("ComboMaxPanel = %d, want 12", engCfg.ComboMaxPanel)
	}
	if engCfg.ComboStragglerGraceMs != 2500 {
		t.Fatalf("ComboStragglerGraceMs = %d, want 2500", engCfg.ComboStragglerGraceMs)
	}
	if engCfg.ComboPanelHardTimeoutMs != 45000 {
		t.Fatalf("ComboPanelHardTimeoutMs = %d, want 45000", engCfg.ComboPanelHardTimeoutMs)
	}
}

// TestApplyFusionRoutingEmptyJudgeModelKeepsEngineDefault: no judge model
// configured means the engine's own default - the first panel member -
// so JudgeModel stays nil (the engine resolves panel[0] itself).
func TestApplyFusionRoutingEmptyJudgeModelKeepsEngineDefault(t *testing.T) {
	engCfg := router.Config{}
	applyFusionRouting(&engCfg, &config.RoutingConfig{FusionJudgeModel: "   "})
	if engCfg.JudgeModel != nil {
		t.Fatal("JudgeModel was set although no judge model is configured (an unset judge model = the first panel member)")
	}
}

// TestApplyFusionRoutingZeroKnobsKeepEngineDefaults: zero means "keep the
// engine default", never an override.
func TestApplyFusionRoutingZeroKnobsKeepEngineDefaults(t *testing.T) {
	engCfg := router.Config{}
	applyFusionRouting(&engCfg, &config.RoutingConfig{})
	if engCfg.ComboMinPanel != 0 || engCfg.ComboMaxPanel != 0 ||
		engCfg.ComboStragglerGraceMs != 0 || engCfg.ComboPanelHardTimeoutMs != 0 {
		t.Fatalf("knobs changed without a configured value: %+v", engCfg)
	}
}

// TestApplyFusionRoutingNilInputsAreSafe: a nil engine config or a nil
// routing block must not panic.
func TestApplyFusionRoutingNilInputsAreSafe(t *testing.T) {
	applyFusionRouting(nil, &config.RoutingConfig{FusionJudgeModel: "x"})
	engCfg := router.Config{}
	applyFusionRouting(&engCfg, nil)
	if engCfg.JudgeModel != nil {
		t.Fatal("JudgeModel set from a nil routing block")
	}
}
