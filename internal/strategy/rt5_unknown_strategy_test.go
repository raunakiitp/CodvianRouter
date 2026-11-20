package strategy

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/raunakiitp/codvian-router/internal/router"
	"github.com/raunakiitp/codvian-router/internal/scoring"
)

// captureSlog installs a slog default logger writing WARN+ text lines
// to buf for the duration of the test, then restores the previous
// default. The factories log through the slog default logger (the
// least-invasive seam: no factory signature changes), so the test
// asserts on the captured output.
func captureSlog(t *testing.T, buf *bytes.Buffer) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// TestUnknownStrategyLogsOneWarning verifies RT-5: an unknown strategy
// name still falls back to the priority (identity) hook, and emits
// exactly one warning naming the unknown string and the fallback;
// a known name emits no warning.
func TestUnknownStrategyLogsOneWarning(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	in := strategyChain()
	got := factory("no-such-strategy")(in)
	if len(got) != len(in) {
		t.Fatalf("unknown-strategy ranked %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("unknown strategy order %v != declared order %v (want priority fallback)", got, in)
		}
	}
	out := buf.String()
	if n := strings.Count(out, "unknown strategy name"); n != 1 {
		t.Fatalf("want exactly 1 unknown-strategy warning, got %d in %q", n, out)
	}
	if !strings.Contains(out, "no-such-strategy") {
		t.Fatalf("warning does not name the unknown string: %q", out)
	}
	if !strings.Contains(out, "priority") {
		t.Fatalf("warning does not name the priority fallback: %q", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Fatalf("warning is not at warning level: %q", out)
	}
}

// TestUnknownStrategyLogsOneWarningWithCacheAffinity is the same
// acceptance check for the live-path factory,
// HookFactoryWithCacheAffinity.
func TestUnknownStrategyLogsOneWarningWithCacheAffinity(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	factory := HookFactoryWithCacheAffinity(strategySignalsFn, scoring.Weights{}, nil)
	in := strategyChain()
	got := factory("bogus-strategy-rt5", router.StrategyHookContext{})(in)
	if len(got) != len(in) {
		t.Fatalf("unknown-strategy ranked %d, want %d", len(got), len(in))
	}
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("unknown strategy order %v != declared order %v (want priority fallback)", got, in)
		}
	}
	out := buf.String()
	if n := strings.Count(out, "unknown strategy name"); n != 1 {
		t.Fatalf("want exactly 1 unknown-strategy warning, got %d in %q", n, out)
	}
	if !strings.Contains(out, "bogus-strategy-rt5") {
		t.Fatalf("warning does not name the unknown string: %q", out)
	}
	if !strings.Contains(out, "priority") {
		t.Fatalf("warning does not name the priority fallback: %q", out)
	}
	if !strings.Contains(out, "WARN") {
		t.Fatalf("warning is not at warning level: %q", out)
	}
}

// TestNilSignalsEmitsNoWarning pins the RT-5 nil-signals early
// return: with a nil signals func both factories return the
// identity hook before Parse is ever reached, so an unknown name
// must emit nothing — no warning, no fallback log at all.
func TestNilSignalsEmitsNoWarning(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	in := strategyChain()
	factory := HookFactory(nil, scoring.Weights{})
	got := factory("bogus")(in)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("nil-signals HookFactory order %v != declared order %v (want identity)", got, in)
		}
	}

	affinityFactory := HookFactoryWithCacheAffinity(nil, scoring.Weights{}, nil)
	got = affinityFactory("bogus", router.StrategyHookContext{})(in)
	for i := range in {
		if got[i] != in[i] {
			t.Fatalf("nil-signals affinity factory order %v != declared order %v (want identity)", got, in)
		}
	}

	if out := buf.String(); out != "" {
		t.Fatalf("nil signals must emit nothing, got %q", out)
	}
}

// TestUnknownStrategyWarnsOncePerInvocation pins one warning per
// invocation: calling the same factory twice with the same unknown
// name emits exactly two warnings (no dedup, no fan-out).
func TestUnknownStrategyWarnsOncePerInvocation(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	_ = factory("bogus-strategy-rt5")(strategyChain())
	_ = factory("bogus-strategy-rt5")(strategyChain())
	if out := buf.String(); strings.Count(out, "unknown strategy name") != 2 {
		t.Fatalf("want exactly 2 unknown-strategy warnings for 2 invocations, got %q", out)
	}

	buf.Reset()
	affinityFactory := HookFactoryWithCacheAffinity(strategySignalsFn, scoring.Weights{}, nil)
	_ = affinityFactory("bogus-strategy-rt5", router.StrategyHookContext{})(strategyChain())
	_ = affinityFactory("bogus-strategy-rt5", router.StrategyHookContext{})(strategyChain())
	if out := buf.String(); strings.Count(out, "unknown strategy name") != 2 {
		t.Fatalf("want exactly 2 unknown-strategy warnings for 2 affinity invocations, got %q", out)
	}
}

// TestKnownStrategyEmitsNoWarning pins the negative half of RT-5: a
// known name (including "priority" itself, which also returns the
// identity hook) must not log.
func TestKnownStrategyEmitsNoWarning(t *testing.T) {
	var buf bytes.Buffer
	captureSlog(t, &buf)

	factory := HookFactory(strategySignalsFn, scoring.Weights{})
	_ = factory("cheapest")(strategyChain())
	_ = factory("priority")(strategyChain())

	affinityFactory := HookFactoryWithCacheAffinity(strategySignalsFn, scoring.Weights{}, nil)
	_ = affinityFactory("balanced", router.StrategyHookContext{})(strategyChain())
	_ = affinityFactory("priority", router.StrategyHookContext{})(strategyChain())

	if out := buf.String(); out != "" {
		t.Fatalf("known strategy names must not log, got %q", out)
	}
}
