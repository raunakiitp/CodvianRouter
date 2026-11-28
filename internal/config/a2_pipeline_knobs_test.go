// A2-7: the pipeline knobs are real config-file fields — they decode,
// validate, and reject typos at load. A request never routes at a
// silently ignored pipeline setting.
package config

import (
	"strings"
	"testing"
)

// TestPipelineKnobsValidateAndReject: the four pipeline fields are
// accepted with valid values and rejected with a typo at load.
func TestPipelineKnobsValidateAndReject(t *testing.T) {
	ok := func(t *testing.T, mut func(*RoutingConfig)) {
		t.Helper()
		cfg := DefaultConfig(t.TempDir())
		mut(cfg.Routing)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate = %v, want nil", err)
		}
	}
	// Valid values.
	ok(t, func(rc *RoutingConfig) {
		rc.PipelineTaskDetection = "auto"
		rc.PipelineSkipTokensUnder = 500
		rc.PipelineFallback = "single-provider"
		rc.PipelineMaxReflectionLoops = 2 // NOT-APPLICABLE but accepted
	})
	ok(t, func(rc *RoutingConfig) { rc.PipelineTaskDetection = "" })
	ok(t, func(rc *RoutingConfig) { rc.PipelineFallback = "" })

	// Rejected typos.
	reject := func(t *testing.T, mut func(*RoutingConfig), want string) {
		t.Helper()
		cfg := DefaultConfig(t.TempDir())
		mut(cfg.Routing)
		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate = nil, want it to reject %q", want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to name %q", err, want)
		}
	}
	reject(t, func(rc *RoutingConfig) { rc.PipelineTaskDetection = "bogus" }, "pipeline_task_detection")
	reject(t, func(rc *RoutingConfig) { rc.PipelineFallback = "bogus" }, "pipeline_fallback")
	reject(t, func(rc *RoutingConfig) { rc.PipelineSkipTokensUnder = -1 }, "pipeline_skip_for_tokens_under")
	reject(t, func(rc *RoutingConfig) { rc.PipelineMaxReflectionLoops = -1 }, "pipeline_max_reflection_loops")
}
